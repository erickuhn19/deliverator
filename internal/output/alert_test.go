package output

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEmitterFiresMatchingCategoryOnly(t *testing.T) {
	got := make(chan AlertEvent, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e AlertEvent
		_ = json.NewDecoder(r.Body).Decode(&e)
		got <- e
		w.WriteHeader(200)
	}))
	defer srv.Close()

	e := NewEmitter(srv.URL, []string{"auth"}, 2)
	e.Fire(AlertEvent{Category: "auth", Code: "no_key", Cmd: "buy", ExitCode: 30}) // matches → fires
	select {
	case ev := <-got:
		if ev.Category != "auth" || ev.Cmd != "buy" || ev.ExitCode != 30 {
			t.Fatalf("bad event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected an alert POST for a matching category")
	}

	e.Fire(AlertEvent{Category: "risk", Cmd: "buy"}) // not in set → must not fire
	select {
	case ev := <-got:
		t.Fatalf("risk should not fire (not in configured set): %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestEmitterDisabledIsNoop(t *testing.T) {
	var nilE *Emitter
	nilE.Fire(AlertEvent{Category: "auth"})       // must not panic
	nilE.FireAlways(AlertEvent{Category: "auth"}) // must not panic
	d := NewEmitter("", nil, 0)
	if d.Enabled() {
		t.Fatal("empty url must be disabled")
	}
	d.Fire(AlertEvent{Category: "auth"})       // no-op
	d.FireAlways(AlertEvent{Category: "auth"}) // no-op
}

func TestEmitterFireAlwaysBypassesCategory(t *testing.T) {
	got := make(chan AlertEvent, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e AlertEvent
		_ = json.NewDecoder(r.Body).Decode(&e)
		got <- e
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// "risk" is NOT in the configured set, so Fire would drop it — FireAlways sends.
	e := NewEmitter(srv.URL, []string{"auth"}, 2)
	e.FireAlways(AlertEvent{Category: "risk", Code: "watch_breach", Cmd: "watch"})
	select {
	case ev := <-got:
		if ev.Code != "watch_breach" || ev.Cmd != "watch" {
			t.Fatalf("bad event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FireAlways should POST regardless of category set")
	}
}

func TestEmitterDefaultCategories(t *testing.T) {
	e := NewEmitter("http://example.invalid", nil, 0)
	if !e.categories["halt"] || !e.categories["auth"] || !e.categories["timeout"] {
		t.Fatalf("default categories should be halt/auth/timeout, got %v", e.categories)
	}
	if e.categories["risk"] {
		t.Fatal("risk must not be in the default set")
	}
}
