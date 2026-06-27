package core

import (
	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/state"
)

// Audit helpers for the safety-lever transitions that don't go through a signing
// Client method (#49): halt is a local file marker (offline, no client) and the
// dead-man-switch ops live in the cmd layer. These append a first-class row to the
// configured audit log so a post-mortem can explain why trading stopped/resumed —
// previously SetHalt and the dms ops left no trail (panic is already audited).
//
// Best-effort + respects --no-audit and state.audit, mirroring the client's audit.

func auditTransition(cfg *config.Config, noAudit bool, entry map[string]any) {
	state.NewAudit(config.ExpandPath(cfg.State.AuditPath), cfg.State.Audit && !noAudit).Append(entry)
}

// AuditHalt records a global-halt transition: {action:"halt", state:"on"|"off"}.
func AuditHalt(cfg *config.Config, noAudit bool, on bool) {
	s := "off"
	if on {
		s = "on"
	}
	auditTransition(cfg, noAudit, map[string]any{"action": "halt", "state": s})
}

// AuditDMS records a dead-man-switch transition: {action:"dms", op:"set"|
// "heartbeat"|"clear", secs?, deadline_ms?}.
func AuditDMS(cfg *config.Config, noAudit bool, op string, secs int, deadlineMs int64) {
	e := map[string]any{"action": "dms", "op": op}
	if secs > 0 {
		e["secs"] = secs
	}
	if deadlineMs > 0 {
		e["deadline_ms"] = deadlineMs
	}
	auditTransition(cfg, noAudit, e)
}
