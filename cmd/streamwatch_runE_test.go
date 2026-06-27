package cmd

// RunE coverage for the callback-driven stream + watch command groups (#102).
//
// stream/watch don't fit the single-envelope runCmd helper: a stream emits one
// envelope per forwarded frame and watch emits a watch.start announce line plus
// one envelope per eval/trigger. So the multi-line happy paths capture the raw
// NDJSON output buffer here and assert on the emitted lines; single-emit error /
// validation paths still go through runCmd. Each test owns a tiny fake that
// overrides only the methods the handler touches.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/output"
)

// swMeta builds a one-coin MetaStore so streamCoin can resolve BTC.
func swMeta() *core.MetaStore {
	return core.NewMetaStore("testnet",
		&hl.Meta{Universe: []hl.AssetInfo{{Name: "BTC", SzDecimals: 5, MaxLeverage: 40}}},
		&hl.SpotMeta{}, time.Now())
}

// swCapture routes JSON output to a fresh buffer and ensures Cfg is non-nil, so
// the multi-line emitters (stream frames, watch announce/eval/trigger) can be
// inspected line by line. Returns the buffer; restores global output on cleanup.
func swCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	if Cfg == nil {
		Cfg = config.Default()
		t.Cleanup(func() { Cfg = nil })
	}
	var buf bytes.Buffer
	output.Configure(true, &buf)
	t.Cleanup(func() { output.Configure(true, nil) })
	return &buf
}

// swLines splits captured NDJSON into decoded envelopes (one per non-empty line).
func swLines(t *testing.T, buf *bytes.Buffer) []envelope {
	t.Helper()
	var out []envelope
	for _, ln := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var env envelope
		if err := json.Unmarshal([]byte(ln), &env); err != nil {
			t.Fatalf("line is not a JSON envelope: %q: %v", ln, err)
		}
		out = append(out, env)
	}
	return out
}

// --- stream: helper-level coverage (streamCoin / streamUser) ---

type swMetaClient struct {
	core.ClientAPI
	addr string
}

func (c swMetaClient) Meta() *core.MetaStore { return swMeta() }
func (c swMetaClient) QueryAddr() string     { return c.addr }

func TestStreamCoinResolveAndUnknown(t *testing.T) {
	c := swMetaClient{}
	coin, err := streamCoin(c, "btc") // case-insensitive resolve
	if err != nil || coin != "BTC" {
		t.Fatalf("streamCoin(btc) = %q, %v; want BTC, nil", coin, err)
	}
	_, err = streamCoin(c, "NOPE")
	oe := asError(err)
	if err == nil || oe.Code != "unknown_coin" || oe.ExitCode() != output.ExitValidation {
		t.Fatalf("unknown coin must be validation/unknown_coin, got %v", err)
	}
}

func TestStreamUserAddrAndMissing(t *testing.T) {
	addr, err := streamUser(swMetaClient{addr: "0xabc"})
	if err != nil || addr != "0xabc" {
		t.Fatalf("streamUser = %q, %v; want 0xabc, nil", addr, err)
	}
	_, err = streamUser(swMetaClient{addr: ""})
	oe := asError(err)
	if err == nil || oe.Code != "no_address" || oe.ExitCode() != output.ExitAuth {
		t.Fatalf("missing addr must be auth/no_address, got %v", err)
	}
}

// --- stream: full RunE happy path (coin stream forwards frames) ---

type swStreamClient struct {
	core.ClientAPI
	addr      string
	gotSubs   []core.StreamSub
	frames    []core.StreamEvent
	streamErr error
}

func (c *swStreamClient) Meta() *core.MetaStore { return swMeta() }
func (c *swStreamClient) QueryAddr() string     { return c.addr }
func (c *swStreamClient) Stream(_ context.Context, subs []core.StreamSub, onEvent func(core.StreamEvent)) error {
	c.gotSubs = subs
	for _, f := range c.frames {
		onEvent(f)
	}
	return c.streamErr
}

// A coin stream (book) resolves the coin, subscribes, and emits one envelope per
// forwarded frame under cmd "stream.book".
func TestStreamBookForwardsFrames(t *testing.T) {
	fake := &swStreamClient{frames: []core.StreamEvent{
		{Channel: "l2Book", Data: json.RawMessage(`{"px":"1"}`)},
		{Channel: "l2Book", Data: json.RawMessage(`{"px":"2"}`)},
	}}
	withFakeClient(t, fake)
	buf := swCapture(t)

	book := swFindStreamSub(t, "book")
	if err := book.RunE(book, []string{"btc"}); err != nil {
		t.Fatalf("stream book should run clean, got %v", err)
	}
	// The resolved coin (BTC) reached the fake via the built sub.
	if len(fake.gotSubs) != 1 || fake.gotSubs[0].Coin != "BTC" || fake.gotSubs[0].Type != core.ChanL2Book {
		t.Fatalf("subs reaching Stream wrong: %+v", fake.gotSubs)
	}
	lines := swLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("want 2 frame envelopes, got %d: %s", len(lines), buf.String())
	}
	for _, ln := range lines {
		if !ln.OK || ln.Cmd != "stream.book" {
			t.Fatalf("frame envelope wrong: %+v", ln)
		}
		if !bytes.Contains(ln.Data, []byte("l2Book")) {
			t.Fatalf("frame data should carry the channel, got %s", ln.Data)
		}
	}
}

// A candle stream threads the --interval flag into the subscription.
func TestStreamCandlesPassesInterval(t *testing.T) {
	fake := &swStreamClient{}
	withFakeClient(t, fake)
	_ = swCapture(t)

	candles := swFindStreamSub(t, "candles")
	prev := sInterval
	sInterval = "5m"
	t.Cleanup(func() { sInterval = prev })

	if err := candles.RunE(candles, []string{"btc"}); err != nil {
		t.Fatalf("stream candles should run clean, got %v", err)
	}
	if len(fake.gotSubs) != 1 || fake.gotSubs[0].Type != core.ChanCandle || fake.gotSubs[0].Interval != "5m" {
		t.Fatalf("candle sub should carry interval 5m: %+v", fake.gotSubs)
	}
}

// A coin+user stream (active-asset) needs both a resolved coin and the address.
func TestStreamActiveAssetNeedsCoinAndUser(t *testing.T) {
	fake := &swStreamClient{addr: "0xdead"}
	withFakeClient(t, fake)
	_ = swCapture(t)

	aa := swFindStreamSub(t, "active-asset")
	if err := aa.RunE(aa, []string{"btc"}); err != nil {
		t.Fatalf("stream active-asset should run clean, got %v", err)
	}
	if len(fake.gotSubs) != 1 || fake.gotSubs[0].Coin != "BTC" || fake.gotSubs[0].User != "0xdead" ||
		fake.gotSubs[0].Type != core.ChanActiveAssetData {
		t.Fatalf("active-asset sub should carry coin+user: %+v", fake.gotSubs)
	}
}

// The events stream fans out into four user-keyed subscriptions.
func TestStreamEventsFansOut(t *testing.T) {
	fake := &swStreamClient{addr: "0xfeed"}
	withFakeClient(t, fake)
	_ = swCapture(t)

	if err := streamEventsCmd.RunE(streamEventsCmd, nil); err != nil {
		t.Fatalf("stream events should run clean, got %v", err)
	}
	if len(fake.gotSubs) != 4 {
		t.Fatalf("events should build 4 subs, got %+v", fake.gotSubs)
	}
	for _, s := range fake.gotSubs {
		if s.User != "0xfeed" {
			t.Fatalf("every events sub must carry the user: %+v", s)
		}
	}
}

// mids is a no-arg coin-less stream — its sub is allMids and it forwards frames.
func TestStreamMidsForwards(t *testing.T) {
	fake := &swStreamClient{frames: []core.StreamEvent{{Channel: "allMids", Data: json.RawMessage(`{}`)}}}
	withFakeClient(t, fake)
	buf := swCapture(t)

	if err := streamMidsCmd.RunE(streamMidsCmd, nil); err != nil {
		t.Fatalf("stream mids should run clean, got %v", err)
	}
	if len(fake.gotSubs) != 1 || fake.gotSubs[0].Type != core.ChanAllMids {
		t.Fatalf("mids sub wrong: %+v", fake.gotSubs)
	}
	if lines := swLines(t, buf); len(lines) != 1 || lines[0].Cmd != "stream.mids" {
		t.Fatalf("mids frame envelope wrong: %+v", lines)
	}
}

// --- stream: error paths ---

// A Stream method error flows through fail() to a categorized failure envelope +
// exit code (the failure envelope is the only line emitted here).
func TestStreamMethodError(t *testing.T) {
	fake := &swStreamClient{streamErr: output.Network("ws_down", "socket dead")}
	withFakeClient(t, fake)

	book := swFindStreamSub(t, "book")
	env, err := runCmd(t, book, []string{"btc"})
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitNetwork {
		t.Fatalf("Stream error must be network CmdError, got %T %v", err, err)
	}
	if env.OK || env.Cmd != "stream.book" || env.Error.Category != "network" || env.Error.Code != "ws_down" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// A user stream with no master address resolves to an auth failure (build step
// fails before Stream is ever called).
func TestStreamUserNoAddrFails(t *testing.T) {
	fake := &swStreamClient{addr: ""}
	withFakeClient(t, fake)

	fills := swFindStreamSub(t, "fills")
	env, err := runCmd(t, fills, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitAuth {
		t.Fatalf("no-addr user stream must be auth CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Code != "no_address" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// An unknown coin on a coin stream is a validation failure before Stream.
func TestStreamUnknownCoinFails(t *testing.T) {
	fake := &swStreamClient{}
	withFakeClient(t, fake)

	book := swFindStreamSub(t, "book")
	env, err := runCmd(t, book, []string{"NOPE"})
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitValidation {
		t.Fatalf("unknown coin must be validation CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Code != "unknown_coin" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// A build-client failure surfaces through a stream RunE as a failure envelope.
func TestStreamClientBuildError(t *testing.T) {
	withClientErr(t, output.Auth("no_agent_key", "run onboard"))
	env, err := runCmd(t, streamMidsCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitAuth {
		t.Fatalf("build error must be auth CmdError, got %T %v", err, err)
	}
	if env.OK || env.Cmd != "stream.mids" || env.Error.Code != "no_agent_key" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// swFindStreamSub returns a stream subcommand by name (the coin/user builders are
// constructed in init(), so we pull them off streamCmd rather than rebuild them).
func swFindStreamSub(t *testing.T, name string) *cobra.Command {
	t.Helper()
	for _, sub := range streamCmd.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	t.Fatalf("stream subcommand %q not found", name)
	return nil
}

// --- watch: validation paths (single failure envelope via runCmd) ---

func swSetWatchFlags(t *testing.T, metric, action, coin string, below float64) {
	t.Helper()
	pm, pb, pa, pc := watchMetric, watchBelow, watchAction, watchCoin
	pco, pds := watchCooldown, watchDMSSecs
	watchMetric, watchBelow, watchAction, watchCoin = metric, below, action, coin
	watchCooldown, watchDMSSecs = 0, 0
	t.Cleanup(func() {
		watchMetric, watchBelow, watchAction, watchCoin = pm, pb, pa, pc
		watchCooldown, watchDMSSecs = pco, pds
	})
}

func TestWatchBadMetric(t *testing.T) {
	swSetWatchFlags(t, "vol", "alert", "", 5)
	env, err := runCmd(t, watchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitValidation {
		t.Fatalf("bad metric must be validation CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Code != "bad_metric" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

func TestWatchBadThreshold(t *testing.T) {
	swSetWatchFlags(t, "liq_distance_pct", "alert", "", 0)
	env, err := runCmd(t, watchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitValidation {
		t.Fatalf("bad threshold must be validation CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Code != "bad_threshold" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

func TestWatchBadAction(t *testing.T) {
	swSetWatchFlags(t, "liq_distance_pct", "explode", "", 5)
	env, err := runCmd(t, watchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitValidation {
		t.Fatalf("bad action must be validation CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Code != "bad_action" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

func TestWatchAlertNoWebhook(t *testing.T) {
	swSetWatchFlags(t, "liq_distance_pct", "alert", "", 5)
	// AlertEmitter is nil/disabled in tests → --action alert is rejected.
	prev := AlertEmitter
	AlertEmitter = output.NewEmitter("", nil, 0)
	t.Cleanup(func() { AlertEmitter = prev })

	env, err := runCmd(t, watchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitValidation {
		t.Fatalf("alert w/o webhook must be validation CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Code != "no_webhook" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// --- watch: full RunE happy path (announce + eval + breach trigger) ---

type swWatchClient struct {
	core.ClientAPI
	evals    []core.WatchEval
	breaches []core.WatchEval
	gotCfg   core.WatchConfig
	watchErr error

	schedDeadline *int64
	schedErr      error
}

func (c *swWatchClient) Watch(_ context.Context, cfg core.WatchConfig, onEval func(core.WatchEval), onBreach func(core.WatchEval)) error {
	c.gotCfg = cfg
	for _, e := range c.evals {
		onEval(e)
	}
	for _, b := range c.breaches {
		onBreach(b)
	}
	return c.watchErr
}

func (c *swWatchClient) ScheduleCancel(_ context.Context, deadlineMs *int64) error {
	c.schedDeadline = deadlineMs
	return c.schedErr
}

// runWatch with --action alert: emits watch.start, one watch eval per frame, and
// one watch.trigger per breach. The alert action with a disabled emitter still
// reports fired=true via fireWatchAction (the POST is best-effort).
func TestWatchRunEAnnounceEvalTrigger(t *testing.T) {
	swSetWatchFlags(t, "liq_distance_pct", "alert", "BTC", 6)
	prevEm := AlertEmitter
	AlertEmitter = output.NewEmitter("https://example.test/hook", []string{"risk"}, 1)
	t.Cleanup(func() { AlertEmitter = prevEm })

	fake := &swWatchClient{
		evals:    []core.WatchEval{{Metric: "liq_distance_pct", Value: "8", Threshold: "6", HasValue: true}},
		breaches: []core.WatchEval{{Metric: "liq_distance_pct", Value: "4", Threshold: "6", WorstCoin: "BTC", Breached: true}},
	}
	withFakeClient(t, fake)
	buf := swCapture(t)

	if err := runWatch(watchCmd, nil); err != nil {
		t.Fatalf("watch run should be clean, got %v", err)
	}
	if fake.gotCfg.Below != 6 || fake.gotCfg.Coin != "BTC" || fake.gotCfg.Metric != core.WatchLiqDistancePct {
		t.Fatalf("watch cfg reaching Watch wrong: %+v", fake.gotCfg)
	}
	lines := swLines(t, buf)
	if len(lines) != 3 {
		t.Fatalf("want start+eval+trigger = 3 lines, got %d: %s", len(lines), buf.String())
	}
	if lines[0].Cmd != "watch.start" || !bytes.Contains(lines[0].Data, []byte(`"armed":true`)) {
		t.Fatalf("first line should be the armed announce: %+v", lines[0])
	}
	if lines[1].Cmd != "watch" {
		t.Fatalf("second line should be an eval: %+v", lines[1])
	}
	if lines[2].Cmd != "watch.trigger" || !bytes.Contains(lines[2].Data, []byte(`"fired":true`)) {
		t.Fatalf("third line should be a fired trigger: %s", lines[2].Data)
	}
}

// A Watch method error flows through fail() — emitted AFTER the announce line, so
// the last line is the categorized failure envelope.
func TestWatchMethodError(t *testing.T) {
	swSetWatchFlags(t, "liq_distance_pct", "alert", "", 5)
	prevEm := AlertEmitter
	AlertEmitter = output.NewEmitter("https://example.test/hook", []string{"risk"}, 1)
	t.Cleanup(func() { AlertEmitter = prevEm })

	fake := &swWatchClient{watchErr: output.Network("stream_lost", "ws closed")}
	withFakeClient(t, fake)
	buf := swCapture(t)

	err := runWatch(watchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitNetwork {
		t.Fatalf("Watch error must be network CmdError, got %T %v", err, err)
	}
	lines := swLines(t, buf)
	last := lines[len(lines)-1]
	if last.OK || last.Cmd != "watch" || last.Error.Category != "network" || last.Error.Code != "stream_lost" {
		t.Fatalf("last line should be the failure envelope: %+v", last)
	}
}

// A build-client failure on watch surfaces as an auth failure envelope.
func TestWatchClientBuildError(t *testing.T) {
	swSetWatchFlags(t, "liq_distance_pct", "alert", "", 5)
	prevEm := AlertEmitter
	AlertEmitter = output.NewEmitter("https://example.test/hook", []string{"risk"}, 1)
	t.Cleanup(func() { AlertEmitter = prevEm })

	withClientErr(t, output.Auth("no_agent_key", "run onboard"))
	env, err := runCmd(t, watchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitAuth {
		t.Fatalf("build error must be auth CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Code != "no_agent_key" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// --- watch: fireWatchAction dispatch (per action) ---

func swBreach() core.WatchEval {
	return core.WatchEval{Metric: "liq_distance_pct", Value: "3", Threshold: "5", WorstCoin: "ETH", Breached: true}
}

// --dry-run reports the action without signing — no client method is called.
func TestFireWatchActionDryRun(t *testing.T) {
	prev := flagDryRun
	flagDryRun = true
	t.Cleanup(func() { flagDryRun = prev })

	buf := swCapture(t)
	// An unstubbed fake — calling any method would panic; dry-run must not.
	fireWatchAction(swMetaClient{}, "panic", swBreach())

	lines := swLines(t, buf)
	if len(lines) != 1 || lines[0].Cmd != "watch.trigger" {
		t.Fatalf("dry-run should emit one trigger line: %+v", lines)
	}
	if !bytes.Contains(lines[0].Data, []byte(`"dry_run":true`)) || !bytes.Contains(lines[0].Data, []byte(`"fired":false`)) {
		t.Fatalf("dry-run trigger should report fired=false/dry_run=true: %s", lines[0].Data)
	}
}

// --action panic dispatches to Client.Panic and reports its result.
type swPanicClient struct {
	core.ClientAPI
	res    *core.PanicResult
	err    error
	called bool
}

func (c *swPanicClient) Panic(context.Context) (*core.PanicResult, error) {
	c.called = true
	return c.res, c.err
}

func TestFireWatchActionPanic(t *testing.T) {
	prev := flagDryRun
	flagDryRun = false
	t.Cleanup(func() { flagDryRun = prev })

	buf := swCapture(t)
	fake := &swPanicClient{res: &core.PanicResult{Canceled: 2, Complete: true, Closed: []any{}}}
	fireWatchAction(fake, "panic", swBreach())

	if !fake.called {
		t.Fatal("panic action must call Client.Panic")
	}
	lines := swLines(t, buf)
	if len(lines) != 1 || lines[0].Cmd != "watch.trigger" {
		t.Fatalf("panic should emit one trigger line: %+v", lines)
	}
	if !bytes.Contains(lines[0].Data, []byte(`"fired":true`)) || !bytes.Contains(lines[0].Data, []byte(`"complete":true`)) {
		t.Fatalf("panic trigger should report fired+complete: %s", lines[0].Data)
	}
}

// A failing Panic is reported (fired=false + error) but never aborts the monitor.
func TestFireWatchActionPanicError(t *testing.T) {
	prev := flagDryRun
	flagDryRun = false
	t.Cleanup(func() { flagDryRun = prev })

	buf := swCapture(t)
	fake := &swPanicClient{err: output.Exchange("flatten_failed", "margin")}
	fireWatchAction(fake, "panic", swBreach())

	lines := swLines(t, buf)
	if len(lines) != 1 || !bytes.Contains(lines[0].Data, []byte(`"fired":false`)) {
		t.Fatalf("failed panic should still emit a fired=false trigger: %+v", lines)
	}
}

// --action dms schedules a cancel and records the deadline in the trigger.
func TestFireWatchActionDMS(t *testing.T) {
	prev := flagDryRun
	flagDryRun = false
	t.Cleanup(func() { flagDryRun = prev })

	// Keep dms-state + audit writes out of the real config dir.
	t.Setenv("DELIVERATOR_HOME", t.TempDir())

	pds := watchDMSSecs
	watchDMSSecs = 30 // >= 5 so the action proceeds without config fallback
	t.Cleanup(func() { watchDMSSecs = pds })

	buf := swCapture(t)
	fake := &swWatchClient{}
	fireWatchAction(fake, "dms", swBreach())

	if fake.schedDeadline == nil {
		t.Fatal("dms action must call ScheduleCancel with a deadline")
	}
	lines := swLines(t, buf)
	if len(lines) != 1 || lines[0].Cmd != "watch.trigger" {
		t.Fatalf("dms should emit one trigger line: %+v", lines)
	}
	if !bytes.Contains(lines[0].Data, []byte(`"fired":true`)) || !bytes.Contains(lines[0].Data, []byte(`"dms_secs":30`)) {
		t.Fatalf("dms trigger should report fired + dms_secs=30: %s", lines[0].Data)
	}
}

// A failing ScheduleCancel is reported (fired=false) without crashing the monitor.
func TestFireWatchActionDMSError(t *testing.T) {
	prev := flagDryRun
	flagDryRun = false
	t.Cleanup(func() { flagDryRun = prev })

	pds := watchDMSSecs
	watchDMSSecs = 30
	t.Cleanup(func() { watchDMSSecs = pds })

	buf := swCapture(t)
	fake := &swWatchClient{schedErr: output.Network("sched_down", "unreachable")}
	fireWatchAction(fake, "dms", swBreach())

	lines := swLines(t, buf)
	if len(lines) != 1 || !bytes.Contains(lines[0].Data, []byte(`"fired":false`)) {
		t.Fatalf("failed dms should emit a fired=false trigger: %+v", lines)
	}
}
