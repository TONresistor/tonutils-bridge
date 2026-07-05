package wsbridge

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/xssnick/tonutils-go/tl"
)

func TestSendRawParsesTonnetBroadcast(t *testing.T) {
	// golden tonnet.broadcast bytes from tonnet-messenger internal/broadcast/testdata/vectors.json
	golden := "368b84cec6b4134879b5562e8fe654f94078b112e8a98ba7901f853ae695bed7e0e3910bad049664cfbcda3200000000587b2274797065223a226d7367222c226e69636b223a22766563222c2274657874223a2268656c6c6f207632222c227473223a313735313730303030303030302c22726f6f6d223a22746f6e6e65743a766563746f7273227d00000020d2686840066f10fbc48ccdc0d1cc9f242dd98ac073f6e335868bb071e35354fc2159749877f7cc0b608fb4d13f3fbba856d5bc56deb63ba7a7dc46472cf7dec67bec2b02000000"
	data, err := hex.DecodeString(golden)
	if err != nil {
		t.Fatal(err)
	}
	var obj any
	if _, err := tl.Parse(&obj, data, true); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := obj.(TonnetBroadcast); !ok {
		t.Fatalf("parsed to %T, want TonnetBroadcast", obj)
	}
	reser, err := tl.Serialize(obj, true)
	if err != nil {
		t.Fatalf("re-serialize (this is what SendCustomMessage does): %v", err)
	}
	if !bytes.Equal(data, reser) {
		t.Fatal("parse then serialize must be byte-identical")
	}
}
