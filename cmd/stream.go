package cmd

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

var sInterval string

// streamCmd groups the live WebSocket feeds. Each subcommand emits NDJSON — one
// envelope per line — and runs until interrupted (Ctrl-C). The socket
// auto-reconnects and resubscribes; a control event {channel:"reconnect"} marks
// each drop.
var streamCmd = &cobra.Command{
	Use:   "stream",
	Short: "Live WebSocket feeds as NDJSON (one envelope per line)",
	RunE:  requireSubcommand,
}

// streamPayload nests one decoded frame under the envelope's data.
type streamPayload struct {
	Channel string          `json:"channel"`
	Event   json.RawMessage `json:"event"`
}

// runStream builds a client and forwards frames until the user interrupts.
func runStream(name string, build func(core.ClientAPI) ([]core.StreamSub, error)) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c, err := newClient(ctx)
	if err != nil {
		return fail("stream."+name, err)
	}
	subs, err := build(c)
	if err != nil {
		return fail("stream."+name, err)
	}
	cmdName := "stream." + name
	if serr := c.Stream(ctx, subs, func(ev core.StreamEvent) {
		emit(cmdName, streamPayload{Channel: ev.Channel, Event: ev.Data})
	}); serr != nil {
		return fail(cmdName, serr)
	}
	return nil
}

func streamCoin(c core.ClientAPI, coin string) (string, error) {
	mk, ok := c.Meta().Lookup(coin)
	if !ok {
		return "", output.Validation("unknown_coin", "unknown coin "+coin).
			WithHint("run `deliverator markets`")
	}
	return mk.Coin, nil
}

func streamUser(c core.ClientAPI) (string, error) {
	a := c.QueryAddr()
	if a == "" {
		return "", output.Auth("no_address",
			"user streams need the master address — set wallet.master_address")
	}
	return a, nil
}

func coinStream(name, typ string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " <coin>",
		Short: "Stream " + name,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStream(name, func(c core.ClientAPI) ([]core.StreamSub, error) {
				coin, err := streamCoin(c, args[0])
				if err != nil {
					return nil, err
				}
				sub := core.StreamSub{Type: typ, Coin: coin}
				if typ == core.ChanCandle {
					sub.Interval = sInterval
				}
				return []core.StreamSub{sub}, nil
			})
		},
	}
}

func userStream(name, typ string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: "Stream " + name + " (user)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStream(name, func(c core.ClientAPI) ([]core.StreamSub, error) {
				addr, err := streamUser(c)
				if err != nil {
					return nil, err
				}
				return []core.StreamSub{{Type: typ, User: addr}}, nil
			})
		},
	}
}

// coinUserStream is a subscription keyed by BOTH a coin and the user (e.g.
// activeAssetData: per-user-per-coin leverage/margin/available-to-trade).
func coinUserStream(name, typ string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " <coin>",
		Short: "Stream " + name + " (user + coin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStream(name, func(c core.ClientAPI) ([]core.StreamSub, error) {
				coin, err := streamCoin(c, args[0])
				if err != nil {
					return nil, err
				}
				addr, err := streamUser(c)
				if err != nil {
					return nil, err
				}
				return []core.StreamSub{{Type: typ, Coin: coin, User: addr}}, nil
			})
		},
	}
}

var streamMidsCmd = &cobra.Command{
	Use:   "mids",
	Short: "Stream all mid prices",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStream("mids", func(c core.ClientAPI) ([]core.StreamSub, error) {
			return []core.StreamSub{{Type: core.ChanAllMids}}, nil
		})
	},
}

// events merges the user's fills, order lifecycle, and notifications.
var streamEventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Merged user stream: fills + order updates + notifications",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStream("events", func(c core.ClientAPI) ([]core.StreamSub, error) {
			addr, err := streamUser(c)
			if err != nil {
				return nil, err
			}
			return []core.StreamSub{
				{Type: core.ChanUserFills, User: addr},
				{Type: core.ChanOrderUpdates, User: addr},
				{Type: core.ChanNotification, User: addr},
				{Type: core.ChanUserTwapSliceFills, User: addr},
			}, nil
		})
	},
}

func init() {
	candles := coinStream("candles", core.ChanCandle)
	candles.Flags().StringVar(&sInterval, "interval", "1m", "candle interval")

	// fundings streams the per-asset context (funding rate, OI, oracle, mark).
	fundings := coinStream("fundings", core.ChanActiveAssetCtx)
	fundings.Short = "Stream funding rate / OI / oracle (per-asset context)"

	// activeAssetData: per-user-per-coin leverage / margin / available-to-trade.
	activeAsset := coinUserStream("active-asset", core.ChanActiveAssetData)
	activeAsset.Short = "Stream per-coin leverage / margin / available-to-trade (user + coin)"

	// webData2: heavy per-user aggregate SNAPSHOT (positions + orders + margin).
	webData := userStream("webdata2", core.ChanWebData2)
	webData.Short = "Stream the full per-user account snapshot (positions + orders + margin)"

	// userTwapSliceFills: live execution progress of running TWAPs.
	twapFills := userStream("twap-fills", core.ChanUserTwapSliceFills)
	twapFills.Short = "Stream TWAP slice executions (running-TWAP progress)"

	streamCmd.AddCommand(
		coinStream("book", core.ChanL2Book),
		coinStream("bbo", core.ChanBbo),
		coinStream("trades", core.ChanTrades),
		candles,
		fundings,
		activeAsset,
		streamMidsCmd,
		userStream("fills", core.ChanUserFills),
		userStream("orders", core.ChanOrderUpdates),
		webData,
		twapFills,
		streamEventsCmd,
	)
	rootCmd.AddCommand(streamCmd)
}
