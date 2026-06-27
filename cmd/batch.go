package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

var (
	bFile       string
	gLevels     int
	gFrom       string
	gTo         string
	gSize       string
	gTif        string
	gReduceOnly bool
)

// flexStr unmarshals from either a JSON string ("0.001") or a bare number
// (0.001), keeping the literal token so price/size precision is never lost to a
// float round-trip. Lets batch files be written either way.
type flexStr string

func (f *flexStr) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexStr(s)
		return nil
	}
	*f = flexStr(strings.TrimSpace(string(b)))
	return nil
}

// batchOrderJSON is one element of a batch file. Sizes/prices accept string or
// number. Mirrors the fields of a single buy/sell/order.
type batchOrderJSON struct {
	Coin       string  `json:"coin"`
	Side       string  `json:"side"`
	Size       flexStr `json:"size"`
	Notional   float64 `json:"notional"` // USD notional sizing when size is omitted (#50)
	Limit      flexStr `json:"limit"`
	Tif        string  `json:"tif"`
	ReduceOnly bool    `json:"reduce_only"`
	Cloid      string  `json:"cloid"`
	Slippage   float64 `json:"slippage"`
	Trigger    *struct {
		TriggerPx flexStr `json:"trigger_px"`
		IsMarket  bool    `json:"is_market"`
		Tpsl      string  `json:"tpsl"`
	} `json:"trigger"`
}

func (o batchOrderJSON) toOrderReq() (core.OrderReq, error) {
	side, err := parseSide(o.Side)
	if err != nil {
		return core.OrderReq{}, err
	}
	req := core.OrderReq{
		Coin: o.Coin, Side: side, Size: string(o.Size), Notional: o.Notional, Limit: string(o.Limit),
		Tif: o.Tif, ReduceOnly: o.ReduceOnly, Cloid: o.Cloid, Slippage: o.Slippage,
	}
	if o.Trigger != nil {
		req.Trigger = &core.TriggerReq{
			TriggerPx: string(o.Trigger.TriggerPx), IsMarket: o.Trigger.IsMarket, Tpsl: o.Trigger.Tpsl,
		}
	}
	return req, nil
}

// batchExit derives the process exit code from the per-leg outcomes, matching the
// single-order path's contract: every leg rejected => exchange (50); any leg
// rejected, unacknowledged, or only PARTIALLY filled => partial (60, "inspect
// fills"); otherwise success. A partially-filled IOC leg must not read as a clean
// fill (exit 0) when buy/sell/close would return 60 for the same fill.
func batchExit(results []*core.PlaceResult) error {
	if len(results) == 0 {
		return nil
	}
	rejected, attention := 0, 0
	for _, r := range results {
		switch {
		case r.Status == "rejected":
			rejected++
		case r.Status == "unknown" || r.IsPartial():
			attention++
		}
	}
	switch {
	case rejected == len(results):
		return output.ExitWith(output.ExitExchange) // 50
	case rejected > 0 || attention > 0:
		return output.ExitWith(output.ExitPartial) // 60
	}
	return nil
}

var batchCmd = &cobra.Command{
	Use:   "batch",
	Short: "Place many orders in one signed action (--file orders.json, or stdin)",
	Long: "Place an array of orders in ONE signed action (one nonce, one builder fee).\n" +
		"Each element: {coin, side, size, limit?, tif?, reduce_only?, cloid?, slippage?, trigger?}.\n" +
		"Validation is atomic: if any leg fails locally, nothing is sent. Sizes/prices may be strings or numbers.",
	RunE: func(cmd *cobra.Command, args []string) error {
		raw, err := readBatchInput(bFile)
		if err != nil {
			return fail("batch", output.Validation("read_batch", err.Error()))
		}
		var rows []batchOrderJSON
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fail("batch", output.Validation("bad_json", "could not parse batch JSON: "+err.Error()).
				WithHint(`expected a JSON array, e.g. [{"coin":"BTC","side":"buy","size":"0.001","limit":"60000"}]`))
		}
		reqs := make([]core.OrderReq, 0, len(rows))
		for i, row := range rows {
			r, err := row.toOrderReq()
			if err != nil {
				return fail("batch", output.Validation("bad_order", fmt.Sprintf("order %d: %s", i, err.Error())))
			}
			reqs = append(reqs, r)
		}

		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("batch", err)
		}
		results, warnings, err := c.PlaceBatch(ctx, reqs)
		if err != nil {
			return fail("batch", err)
		}
		emit("batch", results, warnings...)
		return batchExit(results)
	},
}

// modifyJSON is one element of a modify-batch file. Re-price/re-size a resting
// order by oid or cloid; empty size/limit keep the existing value.
type modifyJSON struct {
	Oid   *int64  `json:"oid"`
	Cloid string  `json:"cloid"`
	Size  flexStr `json:"size"`
	Limit flexStr `json:"limit"`
}

var modifyBatchCmd = &cobra.Command{
	Use:   "modify-batch",
	Short: "Modify many resting orders in one signed action (--file modifies.json, or stdin)",
	Long: "Modify an array of resting orders in ONE signed action.\n" +
		"Each element: {oid|cloid, size?, limit?} — empty size/limit keep the current value.\n" +
		"Validation is atomic: if any leg fails locally, nothing is sent. HL drops the builder fee on modify.",
	RunE: func(cmd *cobra.Command, args []string) error {
		raw, err := readBatchInput(bFile)
		if err != nil {
			return fail("modify-batch", output.Validation("read_batch", err.Error()))
		}
		var rows []modifyJSON
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fail("modify-batch", output.Validation("bad_json", "could not parse modify JSON: "+err.Error()).
				WithHint(`expected a JSON array, e.g. [{"oid":123,"limit":"60000"}]`))
		}
		reqs := make([]core.ModifyReq, len(rows))
		for i, r := range rows {
			reqs[i] = core.ModifyReq{Oid: r.Oid, Cloid: r.Cloid, Size: string(r.Size), Limit: string(r.Limit)}
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("modify-batch", err)
		}
		results, warnings, err := c.ModifyBatch(ctx, reqs)
		if err != nil {
			return fail("modify-batch", err)
		}
		emit("modify-batch", results, warnings...)
		return batchExit(results)
	},
}

var gridCmd = &cobra.Command{
	Use:   "grid <coin> <buy|sell>",
	Short: "Place a ladder of limit orders evenly spaced across a price range",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		side, err := parseSide(args[1])
		if err != nil {
			return fail("grid", err)
		}
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("grid", err)
		}
		reqs, err := c.BuildGrid(core.GridReq{
			Coin: args[0], Side: side, Levels: gLevels, FromPx: gFrom, ToPx: gTo,
			TotalSize: gSize, Tif: gTif, ReduceOnly: gReduceOnly,
		})
		if err != nil {
			return fail("grid", err)
		}
		results, warnings, err := c.PlaceBatch(ctx, reqs)
		if err != nil {
			return fail("grid", err)
		}
		emit("grid", results, warnings...)
		return batchExit(results)
	},
}

// readBatchInput reads the batch file, or stdin when path is "" or "-".
func readBatchInput(path string) ([]byte, error) {
	if path == "" || path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func init() {
	batchCmd.Flags().StringVar(&bFile, "file", "", "path to a JSON array of orders (default: stdin)")
	modifyBatchCmd.Flags().StringVar(&bFile, "file", "", "path to a JSON array of modifies (default: stdin)")

	gridCmd.Flags().IntVar(&gLevels, "levels", 0, "number of price levels (orders)")
	gridCmd.Flags().StringVar(&gFrom, "from", "", "price of the first level")
	gridCmd.Flags().StringVar(&gTo, "to", "", "price of the last level (inclusive)")
	gridCmd.Flags().StringVar(&gSize, "size", "", "total size split evenly across levels")
	gridCmd.Flags().StringVar(&gTif, "tif", "Gtc", "time-in-force for every level: Gtc|Alo|Ioc")
	gridCmd.Flags().BoolVar(&gReduceOnly, "reduce-only", false, "place reduce-only levels")
	for _, f := range []string{"levels", "from", "to", "size"} {
		_ = gridCmd.MarkFlagRequired(f)
	}

	rootCmd.AddCommand(batchCmd, gridCmd, modifyBatchCmd)
}
