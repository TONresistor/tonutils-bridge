package wsbridge

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"github.com/xssnick/tonutils-go/ton"
)

func (b *WSBridge) handleSubscribeTransactions(client *wsClient, req *WSRequest) {
	if atomic.AddInt32(&client.activeSubs, 1) > maxSubscriptionsPerClient {
		atomic.AddInt32(&client.activeSubs, -1)
		b.sendError(client, req.ID, "too many subscriptions", -32602)
		return
	}
	defer atomic.AddInt32(&client.activeSubs, -1)

	var params struct {
		Address    string   `json:"address"`
		LastLT     string   `json:"last_lt"`
		Operations []uint32 `json:"operations,omitempty"` // optional list of opcodes to filter
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

	// Send subscription confirmation
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
			// Opcode filter: skip tx if its in_msg opcode doesn't match any requested operation
			if len(params.Operations) > 0 {
				matched := false
				if tx.IO.In != nil {
					payload := tx.IO.In.Msg.Payload()
					if payload != nil {
						opcode, parseErr := payload.BeginParse().LoadUInt(32)
						if parseErr == nil {
							op := uint32(opcode)
							for _, allowed := range params.Operations {
								if op == allowed {
									matched = true
									break
								}
							}
						}
					}
				}
				if !matched {
					continue
				}
			}
			if !b.sendEvent(client, "transaction", serializeTransaction(tx)) {
				return
			}
		}
	}
}

func (b *WSBridge) handleSubscribeBlocks(client *wsClient, req *WSRequest) {
	if atomic.AddInt32(&client.activeSubs, 1) > maxSubscriptionsPerClient {
		atomic.AddInt32(&client.activeSubs, -1)
		b.sendError(client, req.ID, "too many subscriptions", -32602)
		return
	}
	defer atomic.AddInt32(&client.activeSubs, -1)

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

	// Get the current masterchain seqno as starting point
	master, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]interface{}{
		"subscribed":      true,
		"start_seqno":     master.SeqNo,
		"subscription_id": subID,
	})

	lastSeqno := master.SeqNo

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// WaitForBlock delegates to the liteserver — it blocks server-side until
		// the requested seqno exists, avoiding busy polling on our end.
		block, err := b.api.WaitForBlock(lastSeqno + 1).GetMasterchainInfo(ctx)
		if err != nil {
			// Context cancelled means client disconnected — exit silently
			if ctx.Err() != nil {
				return
			}
			// Transient liteserver error — back off and retry
			time.Sleep(time.Second)
			continue
		}

		shards, shardsErr := b.api.GetBlockShardsInfo(ctx, block)
		if shardsErr != nil {
			log.Warn().Err(shardsErr).Msg("failed to get block shards info, shard transactions may be missed")
		}
		var shardResults []map[string]interface{}
		for _, s := range shards {
			shardResults = append(shardResults, map[string]interface{}{
				"workchain": s.Workchain,
				"shard":     fmt.Sprintf("%016x", uint64(s.Shard)),
				"seqno":     s.SeqNo,
			})
		}
		if shardResults == nil {
			shardResults = []map[string]interface{}{}
		}

		if !b.sendEvent(client, "block", map[string]interface{}{
			"seqno":     block.SeqNo,
			"workchain": block.Workchain,
			"shard":     fmt.Sprintf("%016x", uint64(block.Shard)),
			"root_hash": hex.EncodeToString(block.RootHash),
			"file_hash": hex.EncodeToString(block.FileHash),
			"shards":    shardResults,
		}) {
			return
		}

		lastSeqno = block.SeqNo
	}
}

func (b *WSBridge) handleSubscribeAccountState(client *wsClient, req *WSRequest) {
	if atomic.AddInt32(&client.activeSubs, 1) > maxSubscriptionsPerClient {
		atomic.AddInt32(&client.activeSubs, -1)
		b.sendError(client, req.ID, "too many subscriptions", -32602)
		return
	}
	defer atomic.AddInt32(&client.activeSubs, -1)

	var params struct {
		Address string `json:"address"`
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

	// Snapshot initial state to detect changes
	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	acc, err := b.api.GetAccount(ctx, block, addr)
	if err != nil {
		b.sendError(client, req.ID, "failed to get account: "+err.Error())
		return
	}

	lastBalance := "0"
	if acc.State != nil && acc.State.IsValid {
		lastBalance = acc.State.Balance.Nano().String()
	}
	lastTxLT := acc.LastTxLT

	b.sendResult(client, req.ID, map[string]interface{}{
		"subscribed":      true,
		"address":         addr.String(),
		"balance":         lastBalance,
		"last_tx_lt":      fmt.Sprintf("%d", lastTxLT),
		"subscription_id": subID,
	})

	lastSeqno := block.SeqNo

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Wait for next block before checking account state
		newBlock, err := b.api.WaitForBlock(lastSeqno + 1).GetMasterchainInfo(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(time.Second)
			continue
		}
		lastSeqno = newBlock.SeqNo

		newAcc, err := b.api.GetAccount(ctx, newBlock, addr)
		if err != nil {
			continue // transient error — skip this block
		}

		newBalance := "0"
		if newAcc.State != nil && newAcc.State.IsValid {
			newBalance = newAcc.State.Balance.Nano().String()
		}
		newTxLT := newAcc.LastTxLT

		// Only push when something changed
		if newBalance == lastBalance && newTxLT == lastTxLT {
			continue
		}

		var status string
		if newAcc.State == nil || !newAcc.State.IsValid {
			status = "uninit"
		} else {
			switch string(newAcc.State.Status) {
			case "ACTIVE":
				status = "active"
			case "FROZEN":
				status = "frozen"
			default:
				status = "uninit"
			}
		}

		if !b.sendEvent(client, "account_state", map[string]interface{}{
			"address":      addr.String(),
			"balance":      newBalance,
			"status":       status,
			"last_tx_lt":   fmt.Sprintf("%d", newTxLT),
			"last_tx_hash": hex.EncodeToString(newAcc.LastTxHash),
			"block_seqno":  newBlock.SeqNo,
		}) {
			return
		}

		lastBalance = newBalance
		lastTxLT = newTxLT
	}
}

func (b *WSBridge) handleSubscribeNewTransactions(client *wsClient, req *WSRequest) {
	if atomic.AddInt32(&client.activeSubs, 1) > maxSubscriptionsPerClient {
		atomic.AddInt32(&client.activeSubs, -1)
		b.sendError(client, req.ID, "too many subscriptions", -32602)
		return
	}
	defer atomic.AddInt32(&client.activeSubs, -1)

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

	master, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]interface{}{
		"subscribed":      true,
		"start_seqno":     master.SeqNo,
		"subscription_id": subID,
	})

	lastSeqno := master.SeqNo

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		block, err := b.api.WaitForBlock(lastSeqno + 1).GetMasterchainInfo(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(time.Second)
			continue
		}

		// Collect transactions from masterchain block + all shard blocks
		blocks := []*ton.BlockIDExt{block}
		shards, shardsErr := b.api.GetBlockShardsInfo(ctx, block)
		if shardsErr != nil {
			log.Warn().Err(shardsErr).Msg("failed to get block shards info, shard transactions may be missed")
		}
		blocks = append(blocks, shards...)

		for _, blk := range blocks {
			var after *ton.TransactionID3
			for {
				var txList []ton.TransactionShortInfo
				var incomplete bool
				var err error
				if after != nil {
					txList, incomplete, err = b.api.GetBlockTransactionsV2(ctx, blk, 256, after)
				} else {
					txList, incomplete, err = b.api.GetBlockTransactionsV2(ctx, blk, 256)
				}
				if err != nil {
					break
				}

				for _, tx := range txList {
					if !b.sendEvent(client, "new_transaction", map[string]interface{}{
						"account":         hex.EncodeToString(tx.Account),
						"lt":              fmt.Sprintf("%d", tx.LT),
						"hash":            hex.EncodeToString(tx.Hash),
						"block_workchain": blk.Workchain,
						"block_shard":     fmt.Sprintf("%016x", uint64(blk.Shard)),
						"block_seqno":     blk.SeqNo,
					}) {
						return
					}
				}

				if !incomplete || len(txList) == 0 {
					break
				}
				after = txList[len(txList)-1].ID3()

				select {
				case <-ctx.Done():
					return
				default:
				}
			}
		}

		lastSeqno = block.SeqNo
	}
}

func (b *WSBridge) handleSubscribeConfigChanges(client *wsClient, req *WSRequest) {
	if atomic.AddInt32(&client.activeSubs, 1) > maxSubscriptionsPerClient {
		atomic.AddInt32(&client.activeSubs, -1)
		b.sendError(client, req.ID, "too many subscriptions", -32602)
		return
	}
	defer atomic.AddInt32(&client.activeSubs, -1)

	var params struct {
		Params []int32 `json:"params"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}
	if len(params.Params) == 0 {
		b.sendError(client, req.ID, "params list must not be empty", -32602)
		return
	}
	if len(params.Params) > 50 {
		b.sendError(client, req.ID, "too many params (max 50)", -32602)
		return
	}

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

	// Get the current masterchain block as starting point
	master, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	// Fetch initial config values to populate the cache
	cfg, err := b.api.GetBlockchainConfig(ctx, master, params.Params...)
	if err != nil {
		b.sendError(client, req.ID, "failed to get blockchain config: "+err.Error())
		return
	}

	// Cache: param ID → BOC bytes of its cell
	cache := make(map[int32][]byte, len(params.Params))
	for _, id := range params.Params {
		c := cfg.Get(id)
		if c != nil {
			cache[id] = c.ToBOCWithFlags(false)
		}
	}

	b.sendResult(client, req.ID, map[string]interface{}{
		"subscribed":      true,
		"start_seqno":     master.SeqNo,
		"subscription_id": subID,
	})

	lastSeqno := master.SeqNo

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		block, err := b.api.WaitForBlock(lastSeqno + 1).GetMasterchainInfo(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(time.Second)
			continue
		}
		lastSeqno = block.SeqNo

		newCfg, err := b.api.GetBlockchainConfig(ctx, block, params.Params...)
		if err != nil {
			continue // transient error — skip this block
		}

		for _, id := range params.Params {
			newCell := newCfg.Get(id)
			var newBOC []byte
			if newCell != nil {
				newBOC = newCell.ToBOCWithFlags(false)
			}

			oldBOC := cache[id]
			if bytes.Equal(oldBOC, newBOC) {
				continue
			}

			if !b.sendEvent(client, "config_changed", map[string]interface{}{
				"param_id":    id,
				"block_seqno": block.SeqNo,
				"old_value":   base64.StdEncoding.EncodeToString(oldBOC),
				"new_value":   base64.StdEncoding.EncodeToString(newBOC),
			}) {
				return
			}

			cache[id] = newBOC
		}
	}
}

func (b *WSBridge) handleSubscribeMultiAccount(client *wsClient, req *WSRequest) {
	type accountEntry struct {
		Address    string   `json:"address"`
		LastLT     string   `json:"last_lt,omitempty"`
		Operations []uint32 `json:"operations,omitempty"`
	}
	var params struct {
		Accounts []accountEntry `json:"accounts"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}
	if len(params.Accounts) == 0 {
		b.sendError(client, req.ID, "accounts list must not be empty", -32602)
		return
	}
	if len(params.Accounts) > 100 {
		b.sendError(client, req.ID, "too many accounts (max 100)", -32602)
		return
	}

	subCount := int32(len(params.Accounts))
	if atomic.AddInt32(&client.activeSubs, subCount) > maxSubscriptionsPerClient {
		atomic.AddInt32(&client.activeSubs, -subCount)
		b.sendError(client, req.ID, "too many subscriptions", -32602)
		return
	}
	defer atomic.AddInt32(&client.activeSubs, -subCount)

	// Parse all addresses upfront to fail fast on bad input
	addrs := make([]*address.Address, len(params.Accounts))
	for i, entry := range params.Accounts {
		addr, err := parseAddress(entry.Address)
		if err != nil {
			b.sendError(client, req.ID, fmt.Sprintf("invalid address at index %d: %s", i, err.Error()), -32602)
			return
		}
		addrs[i] = addr
	}

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

	b.sendResult(client, req.ID, map[string]interface{}{
		"subscribed":      true,
		"account_count":   len(params.Accounts),
		"subscription_id": subID,
	})

	var wg sync.WaitGroup

	for i, entry := range params.Accounts {
		wg.Add(1)
		go func(addr *address.Address, ops []uint32, lastLTStr string) {
			defer wg.Done()

			var lastLT uint64
			if lastLTStr != "" && lastLTStr != "0" {
				if parsed, parseErr := strconv.ParseUint(lastLTStr, 10, 64); parseErr == nil {
					lastLT = parsed
				}
			}
			if lastLT == 0 {
				if blk, blkErr := b.api.CurrentMasterchainInfo(ctx); blkErr == nil {
					if acc, accErr := b.api.GetAccount(ctx, blk, addr); accErr == nil && acc != nil {
						lastLT = acc.LastTxLT
					}
				}
			}

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
					// Opcode filter: skip tx if its in_msg opcode doesn't match any requested operation
					if len(ops) > 0 {
						matched := false
						if tx.IO.In != nil {
							payload := tx.IO.In.Msg.Payload()
							if payload != nil {
								opcode, parseErr := payload.BeginParse().LoadUInt(32)
								if parseErr == nil {
									op := uint32(opcode)
									for _, allowed := range ops {
										if op == allowed {
											matched = true
											break
										}
									}
								}
							}
						}
						if !matched {
							continue
						}
					}

					txData := serializeTransaction(tx)
					txData["address"] = addr.String()

					if !b.sendEvent(client, "transaction", txData) {
						cancel()
						return
					}
				}
			}
		}(addrs[i], entry.Operations, entry.LastLT)
	}

	wg.Wait()
}

func (b *WSBridge) handleUnsubscribe(client *wsClient, req *WSRequest) {
	var params struct {
		SubscriptionID string `json:"subscription_id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}
	client.subscriptionsMu.Lock()
	cancel, ok := client.subscriptions[params.SubscriptionID]
	if ok {
		cancel()
		delete(client.subscriptions, params.SubscriptionID)
	}
	client.subscriptionsMu.Unlock()
	if !ok {
		b.sendError(client, req.ID, "subscription not found: "+params.SubscriptionID, -32602)
		return
	}
	b.sendResult(client, req.ID, map[string]interface{}{
		"unsubscribed":    true,
		"subscription_id": params.SubscriptionID,
	})
}
