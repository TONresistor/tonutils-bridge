package wsbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
)

// pendingMsg represents an internal message that has been sent but not yet
// confirmed on-chain. The resolver goroutine polls the destination account
// until a transaction consuming this message appears.
type pendingMsg struct {
	destAddr *address.Address
	bodyHash []byte // 32 bytes — cell hash of the message body
	depth    int
}

// traceState holds shared mutable state for a single trace operation.
// The pending counter is accessed atomically from multiple resolver goroutines.
type traceState struct {
	traceID    string
	rootAddr   string
	pending    int32         // atomic: number of unresolved messages
	timedOut   int32         // atomic: number of timed-out messages
	maxDepth   int
	msgTimeout time.Duration
}

// handleSubscribeTrace implements the "subscribe.trace" method.
// It subscribes to the root account, and for each incoming transaction it
// traces the full chain of internal messages across accounts and blocks.
func (b *WSBridge) handleSubscribeTrace(client *wsClient, req *WSRequest) {
	// Subscription slot accounting
	if atomic.AddInt32(&client.activeSubs, 1) > 50 {
		atomic.AddInt32(&client.activeSubs, -1)
		b.sendError(client, req.ID, "too many subscriptions (max 50)", -32602)
		return
	}
	defer atomic.AddInt32(&client.activeSubs, -1)

	var params struct {
		Address       string `json:"address"`
		LastLT        string `json:"last_lt"`
		MaxDepth      int    `json:"max_depth"`
		MsgTimeoutSec int    `json:"msg_timeout_sec"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	addr, err := parseAddress(params.Address)
	if err != nil {
		b.sendError(client, req.ID, "invalid address: "+err.Error(), -32602)
		return
	}

	var lastLT uint64
	if params.LastLT != "" && params.LastLT != "0" {
		lastLT, err = strconv.ParseUint(params.LastLT, 10, 64)
		if err != nil {
			b.sendError(client, req.ID, "invalid last_lt: "+err.Error(), -32602)
			return
		}
	}

	maxDepth := params.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 3
	}
	if maxDepth > 10 {
		maxDepth = 10
	}

	msgTimeout := time.Duration(params.MsgTimeoutSec) * time.Second
	if msgTimeout <= 0 {
		msgTimeout = 10 * time.Second
	}
	if msgTimeout > 120*time.Second {
		msgTimeout = 120 * time.Second
	}

	// Register subscription
	subID := fmt.Sprintf("sub_%d", atomic.AddUint64(&client.subCounter, 1))
	ctx, cancel := context.WithCancel(client.ctx)
	client.subscriptionsMu.Lock()
	client.subscriptions[subID] = cancel
	client.subscriptionsMu.Unlock()
	defer func() {
		cancel()
		client.subscriptionsMu.Lock()
		delete(client.subscriptions, subID)
		client.subscriptionsMu.Unlock()
	}()

	// If no last_lt provided, start from the current account state
	// to avoid replaying the entire transaction history.
	if lastLT == 0 {
		block, err := b.api.CurrentMasterchainInfo(ctx)
		if err == nil {
			acc, err := b.api.GetAccount(ctx, block, addr)
			if err == nil {
				lastLT = acc.LastTxLT
			}
		}
	}

	// Confirm subscription
	b.sendResult(client, req.ID, map[string]interface{}{
		"subscribed":      true,
		"address":         addr.String(),
		"subscription_id": subID,
	})

	ch := make(chan *tlb.Transaction, 16)
	go b.api.SubscribeOnTransactions(ctx, addr, lastLT, ch)

	for {
		select {
		case <-ctx.Done():
			return
		case tx, ok := <-ch:
			if !ok {
				return
			}

			traceID := hex.EncodeToString(tx.Hash)

			state := &traceState{
				traceID:    traceID,
				rootAddr:   addr.String(),
				maxDepth:   maxDepth,
				msgTimeout: msgTimeout,
			}

			// Push trace_started with the root transaction
			if !b.sendEvent(client, "trace_started", map[string]interface{}{
				"trace_id":        traceID,
				"root_tx":         serializeTransaction(tx),
				"subscription_id": subID,
			}) {
				return
			}

			// Run the coordinator synchronously per root tx so events
			// for one trace complete before the next begins.
			b.traceCoordinator(ctx, client, tx, state)
		}
	}
}

// traceCoordinator extracts outgoing internal messages from the root
// transaction and spawns resolver goroutines. It then reads new pending
// messages discovered by resolvers and spawns additional resolvers until
// all messages are resolved or timed out.
func (b *WSBridge) traceCoordinator(ctx context.Context, client *wsClient, rootTx *tlb.Transaction, state *traceState) {
	initialMsgs := extractInternalOutMsgs(rootTx, 1)

	newMsgsCh := make(chan pendingMsg, 64)
	sem := make(chan struct{}, 50) // max 50 concurrent resolvers

	var wg sync.WaitGroup

	// spawnResolver launches a single resolver goroutine guarded by the semaphore.
	spawnResolver := func(msg pendingMsg) {
		atomic.AddInt32(&state.pending, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release
			b.resolveMessage(ctx, client, state.traceID, msg, state, newMsgsCh)
		}()
	}

	for _, msg := range initialMsgs {
		spawnResolver(msg)
	}

	// Coordination loop: wait for new messages from resolvers.
	// We close the channel when all resolvers finish.
	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()

	totalTxs := 0
	maxDepthReached := 0

	for {
		select {
		case <-ctx.Done():
			// Client disconnected — drain the channel so resolvers stuck
			// writing to the full newMsgsCh can finish, then wait via doneCh
			// (not wg.Wait directly — another goroutine is already waiting).
			go func() {
				for range newMsgsCh {
				}
			}()
			<-doneCh
			close(newMsgsCh)
			return

		case msg := <-newMsgsCh:
			// A resolver found a matching tx and discovered more out_msgs
			totalTxs++
			if msg.depth > maxDepthReached {
				maxDepthReached = msg.depth
			}
			spawnResolver(msg)

		case <-doneCh:
			// All resolvers finished. Drain any remaining messages from the channel
			// that arrived between the last wg.Done and channel close.
		drainLoop:
			for {
				select {
				case msg := <-newMsgsCh:
					totalTxs++
					if msg.depth > maxDepthReached {
						maxDepthReached = msg.depth
					}
					// These are already resolved — they just generated new children.
					// We need to spawn resolvers for them and wait again.
					spawnResolver(msg)
					// Re-launch the waiter since wg is non-zero again.
					doneCh = make(chan struct{})
					go func() {
						wg.Wait()
						close(doneCh)
					}()
					// Break out of drain, back to main select
					break drainLoop
				default:
					_ = b.sendEvent(client, "trace_complete", map[string]interface{}{
						"trace_id":          state.traceID,
						"total_txs":         totalTxs,
						"max_depth_reached": maxDepthReached,
						"timed_out_count":   int(atomic.LoadInt32(&state.timedOut)),
					})
					return
				}
			}
		}
	}
}

// resolveMessage polls the blockchain for a transaction on destAddr whose
// incoming message body hash matches bodyHash. When found, it pushes a
// trace_tx event and feeds any new internal out_msgs back into newMsgsCh.
func (b *WSBridge) resolveMessage(ctx context.Context, client *wsClient, traceID string, msg pendingMsg, state *traceState, newMsgsCh chan<- pendingMsg) {
	defer atomic.AddInt32(&state.pending, -1)

	// Per-message timeout
	msgCtx, msgCancel := context.WithTimeout(ctx, state.msgTimeout)
	defer msgCancel()

	// Get current masterchain block
	block, err := b.api.CurrentMasterchainInfo(msgCtx)
	if err != nil {
		atomic.AddInt32(&state.timedOut, 1)
		_ = b.sendEvent(client, "trace_timeout", map[string]interface{}{
			"trace_id":  traceID,
			"address":   msg.destAddr.String(),
			"body_hash": hex.EncodeToString(msg.bodyHash),
			"depth":     msg.depth,
		})
		return
	}

	// Get initial account state to track LastTxLT changes
	acc, err := b.api.GetAccount(msgCtx, block, msg.destAddr)
	if err != nil {
		// Account may not exist (uninit) — timeout gracefully
		atomic.AddInt32(&state.timedOut, 1)
		_ = b.sendEvent(client, "trace_timeout", map[string]interface{}{
			"trace_id":  traceID,
			"address":   msg.destAddr.String(),
			"body_hash": hex.EncodeToString(msg.bodyHash),
			"depth":     msg.depth,
		})
		return
	}

	lastLT := acc.LastTxLT

	// Check existing transactions first — the message may already be processed
	if lastLT > 0 && acc.LastTxHash != nil {
		if b.scanForMatch(msgCtx, client, traceID, msg, state, newMsgsCh, acc.LastTxLT, acc.LastTxHash) {
			return
		}
	}

	// Poll loop: wait for new blocks, check if account state changed
	lastSeqno := block.SeqNo
	for {
		select {
		case <-msgCtx.Done():
			atomic.AddInt32(&state.timedOut, 1)
			_ = b.sendEvent(client, "trace_timeout", map[string]interface{}{
				"trace_id":  traceID,
				"address":   msg.destAddr.String(),
				"body_hash": hex.EncodeToString(msg.bodyHash),
				"depth":     msg.depth,
			})
			return
		default:
		}

		newBlock, err := b.api.WaitForBlock(lastSeqno + 1).GetMasterchainInfo(msgCtx)
		if err != nil {
			if msgCtx.Err() != nil {
				atomic.AddInt32(&state.timedOut, 1)
				_ = b.sendEvent(client, "trace_timeout", map[string]interface{}{
					"trace_id":  traceID,
					"address":   msg.destAddr.String(),
					"body_hash": hex.EncodeToString(msg.bodyHash),
					"depth":     msg.depth,
				})
				return
			}
			time.Sleep(time.Second)
			continue
		}
		lastSeqno = newBlock.SeqNo

		newAcc, err := b.api.GetAccount(msgCtx, newBlock, msg.destAddr)
		if err != nil {
			continue // transient error — retry next block
		}

		if newAcc.LastTxLT == lastLT {
			continue // no new transactions on this account
		}

		// New transactions — scan them for our message
		if b.scanForMatch(msgCtx, client, traceID, msg, state, newMsgsCh, newAcc.LastTxLT, newAcc.LastTxHash) {
			return
		}

		lastLT = newAcc.LastTxLT
	}
}

// scanForMatch fetches recent transactions on the destination account and
// checks if any has an inbound message whose body hash matches. Returns true
// if a match was found.
func (b *WSBridge) scanForMatch(ctx context.Context, client *wsClient, traceID string, msg pendingMsg, state *traceState, newMsgsCh chan<- pendingMsg, lt uint64, hash []byte) bool {
	txList, err := b.api.ListTransactions(ctx, msg.destAddr, 10, lt, hash)
	if err != nil {
		log.Debug().Err(err).Str("trace_id", traceID).Msg("trace: ListTransactions failed")
		return false
	}

	for _, tx := range txList {
		if tx.IO.In == nil {
			continue
		}
		inPayload := tx.IO.In.Msg.Payload()
		if inPayload == nil {
			continue
		}

		if !bytes.Equal(inPayload.Hash(), msg.bodyHash) {
			continue
		}

		// Match found — push trace_tx event
		txData := serializeTransaction(tx)
		txData["address"] = msg.destAddr.String()

		if !b.sendEvent(client, "trace_tx", map[string]interface{}{
			"trace_id":    traceID,
			"transaction": txData,
			"depth":       msg.depth,
			"address":     msg.destAddr.String(),
		}) {
			return true // client dead, stop scanning
		}

		// If below max depth, extract internal out_msgs and feed them back
		if msg.depth < state.maxDepth {
			childMsgs := extractInternalOutMsgs(tx, msg.depth+1)
			for _, child := range childMsgs {
				newMsgsCh <- child
			}
		}

		return true
	}

	return false
}

// extractInternalOutMsgs extracts internal outgoing messages from a
// transaction and returns them as pendingMsg values at the given depth.
// External-out messages (which leave the chain) are skipped.
func extractInternalOutMsgs(tx *tlb.Transaction, depth int) []pendingMsg {
	if tx.IO.Out == nil {
		return nil
	}

	outMsgs, err := tx.IO.Out.ToSlice()
	if err != nil {
		return nil
	}

	var result []pendingMsg
	for i := range outMsgs {
		intMsg, ok := outMsgs[i].Msg.(*tlb.InternalMessage)
		if !ok {
			continue // skip external-out messages
		}

		if intMsg.DstAddr == nil {
			continue
		}

		body := intMsg.Body
		if body == nil {
			continue
		}

		result = append(result, pendingMsg{
			destAddr: intMsg.DstAddr,
			bodyHash: body.Hash(),
			depth:    depth,
		})
	}

	return result
}
