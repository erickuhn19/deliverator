package papertrade

import (
	"math/big"
	"strings"
	"testing"
)

func TestNonceStore(t *testing.T) {
	s := NewNonceStore(t.TempDir())
	key := NonceKey(Exchange, "session", "0xABC")
	if !strings.Contains(key, ":999:"+strings.ToLower(Exchange)+":session:0xabc") {
		t.Fatalf("key %s", key)
	}
	a, err := s.Take(key)
	if err != nil {
		t.Fatal(err)
	}
	n, ok := new(big.Int).SetString(a, 10)
	if !ok || n.Sign() <= 0 || new(big.Int).Mod(n, big.NewInt(256)).Sign() != 0 {
		t.Fatalf("fresh nonce %s must be a positive multiple of 256", a)
	}
	b, err := s.Take(key)
	if err != nil {
		t.Fatal(err)
	}
	bn, _ := new(big.Int).SetString(b, 10)
	if bn.Cmp(new(big.Int).Add(n, big.NewInt(1))) != 0 {
		t.Fatalf("second nonce %s, want %s+1", b, a)
	}
	other, err := s.Take(NonceKey(Exchange, "paper", "0xabc"))
	if err != nil {
		t.Fatal(err)
	}
	if other == b {
		t.Fatal("routes must not share a nonce")
	}
}

func TestIntentStoreKeepsCancelApart(t *testing.T) {
	s := NewIntentStore(t.TempDir())
	id := "0x" + strings.Repeat("ab", 32)
	open := StoredIntent{IntentID: id, Action: "openPosition", Path: "/intents", Body: []byte(`{"action":"openPosition"}`), Deadline: "1800000000", User: "0x1"}
	if err := s.Put(open); err != nil {
		t.Fatal(err)
	}
	cancel := open
	cancel.Action = "cancelIntent"
	cancel.Path = "/intents/" + strings.TrimPrefix(id, "0x") + "/cancel"
	cancel.Body = []byte(`{"user":"0x1"}`)
	if err := s.Put(cancel); err != nil {
		t.Fatal(err)
	}
	got, err := s.Find(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "openPosition" || string(got.Body) != `{"action":"openPosition"}` {
		t.Fatalf("find returned %+v", got)
	}
	c, err := s.Get("cancelIntent", strings.ToUpper(id))
	if err != nil || string(c.Body) != `{"user":"0x1"}` {
		t.Fatalf("cancel record %+v %v", c, err)
	}
}
