package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

// buildInfoBody turns ["<type>", "k=v", ...] into a /info request body. A value
// that parses as an integer is sent as a number; "@" is kept as a sentinel to be
// resolved to the query address once a client exists.
func buildInfoBody(args []string) (map[string]any, error) {
	body := map[string]any{"type": args[0]}
	for _, kv := range args[1:] {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			return nil, fmt.Errorf("params must be key=value, got %q", kv)
		}
		k, v := kv[:i], kv[i+1:]
		switch {
		case v == "@":
			body[k] = "@"
		default:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				body[k] = n
			} else {
				body[k] = v
			}
		}
	}
	return body, nil
}

// infoCmd is a raw passthrough to the Hyperliquid /info endpoint: it posts
// {type, ...params} and returns the response verbatim. This is the escape hatch
// for any info endpoint that has no dedicated command — e.g.
//
//	deliverator info historicalOrders user=@
//	deliverator info userTwapSliceFills user=@
//	deliverator info spotMetaAndAssetCtxs
//	deliverator info predictedFundings
//	deliverator info fundingHistory coin=BTC startTime=1750000000000
//	deliverator info tokenDetails tokenId=0x...
//
// "@" expands to the configured query (master/sub) address. A value that parses
// as an integer is sent as a number; everything else is sent as a string.
var infoCmd = &cobra.Command{
	Use:   "info <type> [key=value ...]",
	Short: "Raw Hyperliquid /info query (any info endpoint; @ = your address)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		body, err := buildInfoBody(args)
		if err != nil {
			return fail("info", output.Validation("bad_param", err.Error()))
		}
		return runRead("info", func(ctx context.Context, c core.ClientAPI) (any, error) {
			for k, v := range body {
				if s, ok := v.(string); ok && s == "@" {
					// @ is documented to expand to the configured query address. If
					// none is set, fail with the same auth error (exit 30) every
					// dedicated read gives — not a silent user="" sent to the exchange.
					if err := c.RequireQueryAddr(); err != nil {
						return nil, err
					}
					body[k] = c.QueryAddr()
				}
			}
			return c.RawInfo(ctx, body)
		})
	},
}

func init() { rootCmd.AddCommand(infoCmd) }
