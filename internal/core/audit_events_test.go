package core

import (
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
)

func TestAuditHaltAndDMSTransitions(t *testing.T) {
	testHome(t)
	cfg := config.Default() // AuditPath -> temp HOME, Audit=true

	AuditHalt(cfg, false, true)
	AuditHalt(cfg, false, false)
	AuditDMS(cfg, false, "set", 30, 1700000000000)
	AuditDMS(cfg, false, "heartbeat", 30, 1700000000001)
	AuditDMS(cfg, false, "clear", 0, 0)

	rows := readAudit(t)
	if len(rows) != 5 {
		t.Fatalf("want 5 audit rows, got %d: %v", len(rows), rows)
	}
	if rows[0]["action"] != "halt" || rows[0]["state"] != "on" {
		t.Fatalf("row0 halt-on: %v", rows[0])
	}
	if rows[1]["state"] != "off" {
		t.Fatalf("row1 halt-off: %v", rows[1])
	}
	if rows[2]["action"] != "dms" || rows[2]["op"] != "set" || rows[2]["secs"].(float64) != 30 {
		t.Fatalf("row2 dms-set: %v", rows[2])
	}
	if rows[3]["op"] != "heartbeat" {
		t.Fatalf("row3 dms-heartbeat: %v", rows[3])
	}
	if rows[4]["op"] != "clear" {
		t.Fatalf("row4 dms-clear: %v", rows[4])
	}
	// clear carries no secs/deadline
	if _, has := rows[4]["secs"]; has {
		t.Fatalf("clear must not carry secs: %v", rows[4])
	}

	// --no-audit suppresses
	AuditHalt(cfg, true, true)
	if n := len(readAudit(t)); n != 5 {
		t.Fatalf("--no-audit must not append, got %d rows", n)
	}
}
