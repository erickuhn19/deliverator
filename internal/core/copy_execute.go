package core

import (
	"context"
	"errors"
	"time"

	"github.com/erickuhn19/deliverator/internal/output"
)

// LegResult is the outcome of one executed diff leg.
type LegResult struct {
	Coin   string `json:"coin"`
	Class  string `json:"class"`
	Action string `json:"action"`
	Size   string `json:"size,omitempty"`
	Cloid  string `json:"cloid,omitempty"`
	Oid    int64  `json:"oid,omitempty"`
	Status string `json:"status"` // filled | resting | rejected | unknown
	Error  string `json:"error,omitempty"`
}

// CopyExecuteResult is the execute outcome. UnknownCloids carries any exit-42
// (outcome-unknown) legs — the agent must feed these to `reconcile` next cycle
// rather than blind-resubmit. MirroredNow is passed through for the loop to persist.
type CopyExecuteResult struct {
	Leader        string      `json:"leader"`
	Legs          []LegResult `json:"legs"`
	MirroredNow   []string    `json:"mirrored_now"`
	UnknownCloids []string    `json:"unknown_cloids,omitempty"`
	Executed      int         `json:"executed"`
	Complete      bool        `json:"complete"`
}

// CopyExecute applies the diff legs (already ordered reductions-first) through
// Place/Close — which enforce the account-wide risk gates (#43), so there is no
// copy-specific risk code here. A per-leg failure (e.g. a gate rejection on one
// coin) is isolated and the rest continue; an exit-42 (timeout, outcome-unknown)
// leg STOPS the cycle and is surfaced for next-tick Reconcile.
func (c *Client) CopyExecute(ctx context.Context, diff *CopyDiff, p CopyParams) (*CopyExecuteResult, error) {
	res := &CopyExecuteResult{Leader: diff.Leader, MirroredNow: diff.MirroredNow, Legs: []LegResult{}, Complete: true}
	for _, leg := range diff.Diff {
		if p.MaxOrdersPerCycle > 0 && res.Executed >= p.MaxOrdersPerCycle {
			res.Complete = false // budget hit — remaining legs run next cycle
			break
		}
		lctx, cancel := context.WithTimeout(ctx, c.opts.Timeout+5*time.Second) // per-leg budget
		legs, unknown := c.executeLeg(lctx, leg)
		cancel()
		res.Legs = append(res.Legs, legs...)
		res.Executed++
		for _, lr := range legs {
			if lr.Status == "rejected" {
				res.Complete = false
			}
		}
		if unknown != "" {
			res.UnknownCloids = append(res.UnknownCloids, unknown)
			res.Complete = false
			break // outcome unknown — stop; reconcile next cycle, do not blind-resubmit
		}
	}
	return res, nil
}

func (c *Client) executeLeg(ctx context.Context, leg DiffLeg) ([]LegResult, string) {
	switch leg.Class {
	case "open", "increase":
		return c.copyPlace(ctx, leg, leg.Action, leg.Size, leg.Class)
	case "decrease":
		return c.copyClose(ctx, leg, leg.Size, "decrease")
	case "close":
		return c.copyClose(ctx, leg, "", "close") // full flatten
	case "flip":
		// Flip = close the old side fully, then open the new side at the target size.
		// (Never sized as one gross-crossing order — the opening leg is gated on the
		// target notional, not the crossing size.)
		closeRes, unk := c.copyClose(ctx, leg, "", "flip-close")
		if unk != "" {
			return closeRes, unk
		}
		if len(closeRes) > 0 && closeRes[0].Status == "rejected" {
			return closeRes, "" // couldn't flatten — don't open the new side
		}
		openRes, unk2 := c.copyPlace(ctx, leg, leg.Action, leg.Size, "flip-open")
		return append(closeRes, openRes...), unk2
	}
	return nil, ""
}

func (c *Client) copyPlace(ctx context.Context, leg DiffLeg, action, size, class string) ([]LegResult, string) {
	side := Buy
	if action == "sell" {
		side = Sell
	}
	cloid, gerr := GenCloid()
	if gerr != nil {
		// RNG failure before send: nothing went out, so reject (not "unknown").
		return []LegResult{{Coin: leg.Coin, Class: class, Action: action, Size: size, Status: "rejected", Error: gerr.Error()}}, ""
	}
	r := LegResult{Coin: leg.Coin, Class: class, Action: action, Size: size, Cloid: cloid}
	res, _, err := c.Place(ctx, OrderReq{Coin: leg.Coin, Side: side, Size: size, Cloid: cloid})
	if err != nil {
		if isCopyTimeout(err) {
			r.Status, r.Error = "unknown", err.Error()
			return []LegResult{r}, cloid
		}
		r.Status, r.Error = "rejected", err.Error()
		return []LegResult{r}, ""
	}
	r.Status = res.Status
	if res.Oid != nil {
		r.Oid = *res.Oid
	}
	return []LegResult{r}, ""
}

func (c *Client) copyClose(ctx context.Context, leg DiffLeg, size, class string) ([]LegResult, string) {
	// Close auto-determines the reducing side from the live position; leg.Action
	// already records that side (for a flip, the close and open share a side).
	cloid, gerr := GenCloid()
	if gerr != nil {
		// RNG failure before send: nothing went out, so reject (not "unknown").
		return []LegResult{{Coin: leg.Coin, Class: class, Action: leg.Action, Size: size, Status: "rejected", Error: gerr.Error()}}, ""
	}
	r := LegResult{Coin: leg.Coin, Class: class, Action: leg.Action, Size: size, Cloid: cloid}
	res, _, err := c.Close(ctx, leg.Coin, size, true, "", cloid)
	if err != nil {
		if isCopyTimeout(err) {
			r.Status, r.Error = "unknown", err.Error()
			return []LegResult{r}, cloid
		}
		r.Status, r.Error = "rejected", err.Error()
		return []LegResult{r}, ""
	}
	r.Status = res.Status
	if res.Oid != nil {
		r.Oid = *res.Oid
	}
	return []LegResult{r}, ""
}

func isCopyTimeout(err error) bool {
	var oe *output.Error
	return errors.As(err, &oe) && oe.Category == output.CatTimeout
}
