package output

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestEnvelopeSuccessShape(t *testing.T) {
	var buf bytes.Buffer
	Configure(true, &buf)
	defer Configure(true, nil)

	Emit(Response{Cmd: "demo", Data: map[string]string{"a": "b"}, Meta: Meta{Network: "testnet", Account: "main"}})

	var env Envelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	if env.Schema != SchemaVersion {
		t.Errorf("schema = %q, want %q", env.Schema, SchemaVersion)
	}
	if !env.OK || env.Cmd != "demo" || env.Error != nil {
		t.Errorf("unexpected envelope: %+v", env)
	}
	if env.Warnings == nil {
		t.Errorf("warnings must serialize as [] not null")
	}
	if env.Ts == 0 {
		t.Errorf("ts must be set")
	}
}

func TestFailEnvelopeAndExit(t *testing.T) {
	var buf bytes.Buffer
	Configure(true, &buf)
	defer Configure(true, nil)

	err := Fail("demo", Risk("max_order_notional", "too big").WithHint("smaller"), Meta{Network: "mainnet"})
	ce, ok := err.(*CmdError)
	if !ok || ce.Code != ExitRisk {
		t.Fatalf("Fail returned %v, want *CmdError{20}", err)
	}
	var env Envelope
	if jerr := json.Unmarshal(buf.Bytes(), &env); jerr != nil {
		t.Fatal(jerr)
	}
	if env.OK || env.Error == nil || env.Error.Category != CatRisk || env.Error.Hint != "smaller" {
		t.Errorf("bad failure envelope: %+v", env)
	}
}

func TestExitCodeMapping(t *testing.T) {
	want := map[Category]int{
		CatValidation: 10, CatPrecision: 11, CatRisk: 20, CatHalt: 21, CatAuth: 30,
		CatNetwork: 40, CatRateLimit: 41, CatTimeout: 42, CatExchange: 50,
		CatPartial: 60, CatClock: 70, CatUnknown: 1,
	}
	for cat, code := range want {
		if got := NewError(cat, "c", "m").ExitCode(); got != code {
			t.Errorf("category %s → exit %d, want %d", cat, got, code)
		}
	}
}
