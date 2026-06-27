package cmd

import (
	"reflect"
	"sort"
	"strings"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/hl"
)

// schemaRegistry maps a command name to a zero value of the type its `data`
// payload deserializes to, so `schema <command>` can emit that payload's JSON
// Schema. The envelope schema (schema/ok/ts/cmd/data/error/warnings/meta) is the
// same for every command and is what bare `schema` prints; this describes the
// command-specific `data`. Keys are the names you pass to `schema <name>`.
var schemaRegistry = map[string]any{
	// reads
	"positions":          []core.PositionView{},
	"portfolio":          core.PortfolioView{},
	"balance":            core.BalanceView{},
	"ctx":                core.CtxView{},
	"markets":            []core.Market{},
	"bbo":                core.BboView{},
	"book":               core.BookView{},
	"limits":             core.LimitsView{},
	"preview":            core.PreviewResult{},
	"snapshot":           core.SnapshotView{},
	"reconcile":          core.ReconcileView{},
	"pnl-attribution":    core.PnlAttributionView{},
	"twap-status":        core.TwapStatusView{},
	"fills":              []hl.Fill{},
	"predicted-fundings": []hl.PredictedFunding{},
	"historical-orders":  []hl.OrderQueryResponse{},
	"leaderboard":        core.LeaderboardView{},
	// writes
	"buy":           core.PlaceResult{},
	"sell":          core.PlaceResult{},
	"order":         core.PlaceResult{},
	"cancel":        core.CancelResult{},
	"position-tpsl": []core.PlaceResult{},
	"twap":          core.TwapResult{},
	"leverage":      core.LeverageResult{},
	"margin":        core.MarginResult{},
	"panic":         core.PanicResult{},
	// streams (one envelope per line)
	"watch": core.WatchEval{},
	"chase": core.ChaseEvent{},
}

// describeCommand returns the JSON Schema of a command's `data` payload, and
// whether the command has a registered typed payload.
func describeCommand(name string) (map[string]any, bool) {
	v, ok := schemaRegistry[strings.ToLower(name)]
	if !ok {
		return nil, false
	}
	return jsonSchema(reflect.TypeOf(v), map[reflect.Type]bool{}), true
}

// describableCommands is the sorted list of names that have a typed `data` schema.
func describableCommands() []string {
	out := make([]string, 0, len(schemaRegistry))
	for k := range schemaRegistry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// jsonSchema reflects a Go type into a minimal JSON-Schema description of the
// shape: type + properties/items, with `required` listing the non-omitempty
// fields. It is a documentation / codegen aid, not a full validator. Recursive
// types (e.g. an order's children) are cut with a note rather than expanded
// forever. NB: HL prices/sizes are JSON strings (type "string"); only genuine
// counters/timestamps are numbers.
func jsonSchema(t reflect.Type, seen map[reflect.Type]bool) map[string]any {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": jsonSchema(t.Elem(), seen)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": jsonSchema(t.Elem(), seen)}
	case reflect.Interface:
		return map[string]any{} // any (no constraint)
	case reflect.Struct:
		if seen[t] {
			return map[string]any{"type": "object", "note": "recursive " + t.Name()}
		}
		seen[t] = true
		defer delete(seen, t)
		props := map[string]any{}
		var required []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue // unexported
			}
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			parts := strings.Split(tag, ",")
			name, omitempty := f.Name, false
			if len(parts) > 0 && parts[0] != "" {
				name = parts[0]
			}
			for _, p := range parts[1:] {
				if p == "omitempty" {
					omitempty = true
				}
			}
			// Flatten an embedded struct that has no json name of its own.
			if f.Anonymous && (len(parts) == 0 || parts[0] == "") {
				if sub := jsonSchema(f.Type, seen); sub["type"] == "object" {
					if sp, ok := sub["properties"].(map[string]any); ok {
						for k, v := range sp {
							props[k] = v
						}
						if r, ok := sub["required"].([]string); ok {
							required = append(required, r...)
						}
						continue
					}
				}
			}
			props[name] = jsonSchema(f.Type, seen)
			if !omitempty {
				required = append(required, name)
			}
		}
		out := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			sort.Strings(required)
			out["required"] = required
		}
		return out
	default:
		return map[string]any{"type": "string"}
	}
}
