package core

// Coverage for resolvePriority — the order-priority fee (bps → HL `p` rate)
// resolution: config default, per-order override, clamp-to-cap with warning, off.

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

func TestResolvePriority(t *testing.T) {
	cfg := config.Default() // MaxPriorityBps=8, Automation.PriorityBps=0
	c := newCfgClient(t, cfg)

	// Off by default (no config default, no override).
	if r, w := c.resolvePriority(nil); r != 0 || w != "" {
		t.Fatalf("default should be off, got rate=%d warn=%q", r, w)
	}
	// 3 bps override -> p = 30000 (rate = p/1e8 = 3bps), no clamp.
	three := 3
	if r, w := c.resolvePriority(&three); r != 30000 || w != "" {
		t.Fatalf("3 bps -> rate=%d warn=%q, want 30000/none", r, w)
	}
	// Above HL's 8 bps cap -> clamped to 8 (p=80000) WITH a warning.
	ten := 10
	if r, w := c.resolvePriority(&ten); r != 80000 || w == "" {
		t.Fatalf("10 bps must clamp to 80000 with a warning, got rate=%d warn=%q", r, w)
	}
	// Config default applies when there's no override.
	cfg.Automation.PriorityBps = 2
	if r, _ := c.resolvePriority(nil); r != 20000 {
		t.Fatalf("config default 2 bps -> rate=%d, want 20000", r)
	}
	// A lower configured cap clamps an override down (with a warning).
	cfg.Risk.MaxPriorityBps = 1
	if r, w := c.resolvePriority(&three); r != 10000 || w == "" {
		t.Fatalf("max 1 bp must clamp 3 bps to 10000 with a warning, got rate=%d warn=%q", r, w)
	}
}
