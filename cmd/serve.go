package cmd

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/serve"
)

var serveSocket string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run as a local engine on a Unix socket, so a bot can drive deliverator without forking it per call",
	Long: `Serve the execution path over a Unix domain socket instead of one process per
command.

Fork-per-call is right for a human and wrong for a market maker: every call pays
a process spawn, a fresh TLS handshake and a fresh connection, and a fast quote
loop can exhaust Hyperliquid's per-IP weight budget on that alone. It also makes
a persistent websocket for order entry impossible, because nothing lives long
enough to hold one open.

One server means one client, one connection pool, one nonce holder — and a place
for that websocket to live.

The protocol is NDJSON, one request per line, one envelope per line back:

  {"id":"1","method":"book","params":{"coin":"#9370","levels":20}}
  {"id":"2","method":"place","params":{"coin":"BTC","side":"buy","size":"0.001","limit":"60000","tif":"Alo"}}

` + "`ping`" + ` returns the served method list. Streams stay on the CLI: a market-data
feed is already one long-lived process and is not the churn this addresses.

SECURITY. This socket can SIGN. It is owner-only (0600) and Unix-domain by
design — never a TCP port, and it must not be reachable off the box. There is no
authentication behind the filesystem permissions. Every request still passes the
same risk gates, nonce discipline and audit trail as the CLI: serve mode is a
second front-end onto the engine, not a way around its envelope.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		c, err := newClient(ctx)
		if err != nil {
			return fail("serve", err)
		}
		eng, ok := c.(serve.Engine)
		if !ok {
			return fail("serve", output.Unknown("engine",
				"this client does not implement the served engine surface"))
		}
		// HIP-4 markets rotate daily and load on demand elsewhere. A long-lived
		// server has no argv to inspect for "#" coins, so load them up front —
		// otherwise the first outcome request of the process fails to resolve a
		// coin that plainly exists.
		if err := c.EnsureOutcomes(ctx); err != nil {
			return fail("serve", err)
		}

		srv := serve.New(eng, serveSocketPath(), func() output.Meta { return RootMeta(0) })
		if err := srv.Listen(); err != nil {
			return fail("serve", output.Validation("listen", err.Error()))
		}
		// Announced on stdout as a normal envelope so a supervising bot can block
		// on the first line and know the socket is ready, rather than polling for
		// the file to appear and racing the chmod.
		output.Emit(output.Response{Cmd: "serve", Data: map[string]any{
			"socket":  srv.Addr(),
			"methods": serve.Methods,
			"dry_run": flagDryRun,
		}, Meta: RootMeta(0)})
		if err := srv.Serve(ctx); err != nil {
			return fail("serve", output.Unknown("serve", err.Error()))
		}
		return nil
	},
}

// serveSocketPath resolves where to listen: --socket, else $DELIVERATOR_SOCKET,
// else a per-account default inside the config directory. Per-account so two
// accounts served at once cannot silently share one engine — the socket carries
// signing authority, and crossing them would place orders on the wrong account.
func serveSocketPath() string {
	if serveSocket != "" {
		return config.ExpandPath(serveSocket)
	}
	if p := os.Getenv("DELIVERATOR_SOCKET"); p != "" {
		return config.ExpandPath(p)
	}
	name := "deliverator.sock"
	if flagAccount != "" {
		name = "deliverator-" + flagAccount + ".sock"
	}
	return filepath.Join(config.Dir(), name)
}

func init() {
	serveCmd.Flags().StringVar(&serveSocket, "socket", "",
		"unix socket path (default: $DELIVERATOR_SOCKET, else <config dir>/deliverator[-<account>].sock)")
	rootCmd.AddCommand(serveCmd)
}

// compile-time proof that the production client satisfies the served surface, so
// a signature change in core is a build error here rather than a runtime type
// assertion failing after the socket is already bound.
var _ serve.Engine = (*core.Client)(nil)
