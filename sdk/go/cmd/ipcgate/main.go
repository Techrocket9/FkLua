// Command ipcgate holds one fkipc conversation with a running Factorio and
// says, leg by leg, whether each part of the protocol worked.
//
// It is the companion half of scripts/run-ipc.sh, and it is a REAL BINARY IN
// THE SDK MODULE rather than a test-only main for three reasons, each of which
// on its own would be enough. A `go test` inside this module proves the
// packages compile; it does not prove an outside tool can LINK the SDK and get
// a working program, which is the SDK's entire claim. The gate is a shell
// script and needs something to execute. And a downstream author asking "what
// does a consumer look like" gets a file rather than a paragraph -- this is the
// SDK's worked example, in the same sense guest/go/examples/api is the
// bindings'.
//
//	ipcgate -game-port 25409 -listen-port 25411 -script-output DIR
//
// The two ports MUST differ: --enable-lua-udp binds ONE socket, which is both
// the game's receive socket and the source port of everything it sends, so a
// companion sharing it is the game talking to itself. Dial refuses it; this
// says so earlier and with the flag names.
//
// WHAT IT PRINTS. One `PASS <leg> -- detail` or `FAIL <leg> -- detail` per leg,
// then `RESULT ok` or `RESULT failed N`. The first two fields of each line are
// the run-to-run comparable part and the detail deliberately is not: the epoch
// is entropy this side minted and the tick a datagram lands on is a race
// between a real clock and the game's update loop. See run-ipc.sh's
// determinism section, which compares exactly those two fields.
//
// It never blocks forever. Every wait has its own deadline and the whole run
// has one, because a companion that hangs holds a headless Factorio open, and
// an orphaned server LOCKS the user directory for every later in-game run.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/Techrocket9/fklua/guest/go/fkipc/wire"
	"github.com/Techrocket9/fklua/sdk/go/fkipc"
)

// The example guest's two channels. Splitting telemetry from control is the
// library's own advice: a channel's seq is shared by everything on it, so a
// lost REQ raises a gap -- and therefore a RESYNC and a snapshot -- on whatever
// telemetry shares the channel.
const (
	chanState   = 1
	chanControl = 2

	// The file guest/go/examples/ipc writes when asked, and what is in it: the
	// 256 byte values four times over. Deterministic on purpose -- the peer
	// verifies length and FNV-1a-32 before handing the bytes over, and this
	// then checks the bytes themselves, so a transfer that mangled one fails
	// rather than arriving plausible.
	bulkName  = "fkipc-gate.bin"
	bulkBytes = 1024
)

func main() {
	gamePort := flag.Uint("game-port", 25409, "--enable-lua-udp's port: the game's one socket")
	listenPort := flag.Uint("listen-port", 25411, "ours, and it must differ from -game-port")
	scriptOut := flag.String("script-output", "",
		"where the guest's files land; FACTORIO_USERDIR/script-output when empty")
	timeout := flag.Duration("timeout", 90*time.Second, "the whole conversation's deadline")
	step := flag.Duration("step", 20*time.Second, "one leg's deadline")
	verbose := flag.Bool("v", false, "log the SDK's own diagnostics")
	flag.Parse()

	if *gamePort == *listenPort {
		fmt.Printf("FAIL ports -- -game-port and -listen-port are both %d; "+
			"--enable-lua-udp binds ONE socket and it is also the game's SOURCE port\n",
			*gamePort)
		fmt.Println("RESULT failed 1")
		os.Exit(1)
	}

	lg := slog.New(slog.DiscardHandler)
	if *verbose {
		lg = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	g := &gate{step: *step}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	up := make(chan uint32, 4)
	msgs := make(chan fkipc.Message, 64)
	files := make(chan pickup, 4)

	s, err := fkipc.Dial(fkipc.Options{
		GamePort:     uint16(*gamePort),
		ListenPort:   uint16(*listenPort),
		ScriptOutput: *scriptOut,
		// ONE TOKEN NAMES THE CONTRACT, and both example guests carry the same
		// string. It is the schema filter -- the only one of the four that can
		// refuse a peer whose transport is entirely correct -- and having it set
		// on a matched pairing makes this gate a positive control for the check.
		// The mismatched case is run-ipcdemo.sh --smoke's identity leg.
		Name:         "fk-ipc/1",
		ExpectedName: "fk-ipc/1",
		Logger:       lg,
	})
	if err != nil {
		fmt.Printf("FAIL dial -- %v\n", err)
		fmt.Println("RESULT failed 1")
		os.Exit(2)
	}

	s.OnSession(func(ev fkipc.SessionEvent, epoch uint32) {
		if ev == fkipc.SessionUp {
			select {
			case up <- epoch:
			default:
			}
		}
	})
	s.Subscribe(chanState, func(m fkipc.Message) {
		select {
		case msgs <- m:
		default:
		}
	})
	s.OnFile(func(n fkipc.FileNotify, r io.ReadCloser) {
		b, err := io.ReadAll(r)
		r.Close()
		select {
		case files <- pickup{n, b, err}:
		default:
		}
	})

	// 1. THE SESSION. The guest sends a HELLO every SearchTicks whether or not
	//    anyone is listening -- a guest with no peer must still be able to
	//    notice one appearing -- so this side simply waits for the next one.
	var epoch uint32
	select {
	case epoch = <-up:
		g.pass("session", "epoch %#08x, minted here and adopted by the guest", epoch)
	case <-time.After(g.step):
		g.fail("session", "no HELLO in %s: the guest never spoke, or the ports are crossed", g.step)
		finish(g, s)
	case <-ctx.Done():
		g.fail("session", "%v", ctx.Err())
		finish(g, s)
	}

	// 2. AN RPC ROUND TRIP CARRYING BINARY. All 256 byte values, out through
	//    send_udp's LocalisedString and back through the event encode, the
	//    guest's decode and its handler. NUL must not truncate and a high byte
	//    must not be UTF-8-mangled -- which the probe measured on the transport
	//    and this measures through every layer above it.
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	echo, note, err := ask(ctx, g, s, chanControl, all)
	switch {
	case err != nil:
		g.fail("rpc-binary", "%v", err)
	case !bytes.Equal(echo, all):
		g.fail("rpc-binary", "the echo is %d bytes and differs at %d",
			len(echo), firstDiff(echo, all))
	default:
		g.pass("rpc-binary", "256 byte values echoed byte for byte%s", note)
	}

	// 3. TELEMETRY. The guest sends its state channel every 60 ticks, so this
	//    is also a check that the game is actually TICKING -- a headless server
	//    with nobody connected pauses by default, on_tick never fires, and the
	//    whole protocol reads as broken.
	if m, ok := waitMsg(msgs, g.step, func(m fkipc.Message) bool {
		return !m.Snapshot && bytes.HasPrefix(m.Payload, []byte("tick="))
	}); ok {
		g.pass("telemetry", "MSG seq %d on channel %d: %q", m.Seq, m.Channel, m.Payload)
	} else {
		g.fail("telemetry", "no periodic MSG in %s (auto_pause? the server may not be ticking)", g.step)
	}

	// 3b. THE GUEST'S CLOCK, which is what the peer needs to notice a rollback.
	//
	//     The tick only ever crosses in a HEARTBEAT, and the guest heartbeats
	//     UNCONDITIONALLY once a second of game time -- it used to suppress the
	//     heartbeat whenever anything else had gone out in the window, which for
	//     a guest streaming telemetry meant never, so this reading froze at the
	//     HELLO for the whole session and nothing said so. Two samples a second
	//     and a half apart is the smallest thing that can tell a live clock from
	//     a frozen one.
	first := s.Stats().GuestTick
	time.Sleep(1500 * time.Millisecond)
	if now := s.Stats().GuestTick; now > first {
		g.pass("clock", "the guest's tick advanced %d -> %d in 1.5 s", first, now)
	} else {
		g.fail("clock", "the guest's tick is stuck at %d: no HEARTBEAT is "+
			"carrying it, so this side cannot see the guest time-travel", now)
	}

	// 4. RESYNC -> SNAPSHOT. A gap is answered with a snapshot and never a
	//    replay: the producer usually cannot replay, because the state it
	//    described no longer exists.
	if err := s.Resync(chanState); err != nil {
		g.fail("resync", "sending RESYNC: %v", err)
	} else if m, ok := waitMsg(msgs, g.step, func(m fkipc.Message) bool {
		return m.Snapshot
	}); ok {
		g.pass("resync", "SNAPSHOT seq %d: %q", m.Seq, m.Payload)
	} else {
		g.fail("resync", "no SNAPSHOT in %s", g.step)
	}

	// 5. BULK: a file, and a digest that says it is all there.
	//
	//    NOTHING DOCUMENTS A FLUSH GUARANTEE for helpers.write_file, so a
	//    notify is not "the bytes are on disk". The guest held these bytes and
	//    hashed them, so HAS_DIGEST is set and the pickup's test is exact --
	//    read until Bytes and the checksum matches, or keep waiting. This then
	//    checks the CONTENT, which the digest does not: an FNV collision is
	//    unlikely and a guest that wrote a different deterministic buffer is
	//    not.
	ack, _, err := ask(ctx, g, s, chanControl, []byte("bulk"))
	if err != nil {
		g.fail("bulk", "asking for the file: %v", err)
	} else if string(ack) != "queued" {
		g.fail("bulk", "the guest answered %q rather than \"queued\"", ack)
	} else {
		select {
		case p := <-files:
			checkBulk(g, p)
		case <-time.After(g.step):
			g.fail("bulk", "no FILE_NOTIFY satisfied in %s -- see the SDK's pickup log", g.step)
		}
	}

	// 6. BYE. Advisory: the guest recovers from this side simply vanishing, by
	//    liveness. What it buys is turning a three-second timeout into an
	//    immediate one, which is what the GUEST-side half of this leg checks --
	//    run-ipc.sh greps the game's log for the session going down.
	st := s.Stats()

	// 7. ONE SESSION FOR THE WHOLE RUN, AND THIS LEG IS WHERE P6 IS BURIED.
	//
	//    Starting a headless server LOADS the map, so the guest reaches its
	//    first tick already restored. Under the load-reset design that put TWO
	//    HELLOs on the wire one tick apart -- one carrying the pre-reload boot
	//    and one the post-reload one -- and a HELLO is unconditionally a new
	//    session here, so a companion that happened to be listening early
	//    enough minted two and failed anything in flight with ErrSessionLost.
	//    Whether a run saw it was a RACE between when this side bound its
	//    socket and when the guest first ticked, which is why the count used to
	//    live in the STATS line where nothing compared it: two consecutive runs
	//    of one arm reported sessions=2 and sessions=1.
	//
	//    A load resets nothing now, so the count is determined and this is a
	//    verdict. It can still fail, and what it would mean is the thing worth
	//    catching: something reset a session that nobody tore down.
	if st.Sessions == 1 {
		g.pass("sessions", "exactly one session for the whole run")
	} else {
		g.fail("sessions", "%d sessions: a load must not open one, and nothing "+
			"here tore one down before the BYE", st.Sessions)
	}

	if err := s.Close(); err != nil {
		g.fail("bye", "%v", err)
	} else {
		g.pass("bye", "BYE sent and the socket closed")
	}
	fmt.Printf("STATS epoch=%#08x sessions=%d tx=%d/%dB rx=%d/%dB drops=%d bad=%d stale=%d gaps=%d retries=%d guest_tick=%d guest_boot=%d\n",
		st.Epoch, st.Sessions, st.TxFrames, st.TxBytes, st.RxFrames, st.RxBytes,
		st.Drops, st.BadFrames, st.StaleDrops, st.Gaps, st.Retries, st.GuestTick,
		st.GuestBoot)

	if g.fails == 0 {
		fmt.Println("RESULT ok")
		return
	}
	fmt.Printf("RESULT failed %d\n", g.fails)
	os.Exit(1)
}

// ask is Request plus the ONE recovery the protocol asks an application to
// perform, and it is here in the worked example rather than hidden in the
// library because the distinction is the whole point: ErrSessionLost is not
// "the request failed", it is "THE OUTCOME IS UNKNOWN". Only the application
// knows whether re-asking is safe, which is what "idempotent RPC" buys and why
// the library will never retry across a session boundary by itself. An echo is
// idempotent, so this re-asks once.
//
// IT NO LONGER FIRES IN THIS HARNESS, and it is kept anyway. It used to fire
// for real: starting a headless server LOADS the map, and under the load-reset
// design that put two HELLOs on the wire one tick apart, so anything in flight
// across the boundary died exactly here (agents/ipc.md, P6, now closed). A load
// resets nothing today and the `sessions` leg asserts the count is one.
//
// What it demonstrates is still the thing worth demonstrating: a session CAN
// end under a request -- a BYE, liveness, or this side noticing the guest's
// clock go backwards -- and when it does, the library will not retry, because
// ErrSessionLost means the outcome is UNKNOWN and only the application knows
// whether re-asking is safe. An echo is idempotent, so this re-asks once.
func ask(ctx context.Context, g *gate, s *fkipc.Session, ch uint16, p []byte) ([]byte, string, error) {
	note := ""
	for try := 0; ; try++ {
		rctx, cancel := context.WithTimeout(ctx, g.step)
		out, err := s.Request(rctx, ch, p)
		cancel()
		if try > 0 || !(errors.Is(err, fkipc.ErrSessionLost) || errors.Is(err, fkipc.ErrNoSession)) {
			return out, note, err
		}
		note = " (re-asked across a session boundary)"
		deadline := time.Now().Add(g.step)
		for !s.Stats().Up && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	}
}

type pickup struct {
	n    fkipc.FileNotify
	data []byte
	err  error
}

func checkBulk(g *gate, p pickup) {
	switch {
	case p.err != nil:
		g.fail("bulk", "reading the file: %v", p.err)
	case p.n.Name != bulkName:
		g.fail("bulk", "the notify names %q, not %q", p.n.Name, bulkName)
	case !p.n.HasDigest:
		g.fail("bulk", "no digest: WriteBulk is supposed to set HAS_DIGEST")
	case len(p.data) != bulkBytes:
		g.fail("bulk", "%d bytes, want %d", len(p.data), bulkBytes)
	case wire.FNV1a32(p.data) != p.n.Digest:
		g.fail("bulk", "digest %#08x over the bytes, notify says %#08x",
			wire.FNV1a32(p.data), p.n.Digest)
	default:
		for i, b := range p.data {
			if b != byte(i) {
				g.fail("bulk", "byte %d is %#02x, want %#02x", i, b, byte(i))
				return
			}
		}
		g.pass("bulk", "%d bytes, fnv1a32 %#08x verified, content exact",
			len(p.data), p.n.Digest)
	}
}

// waitMsg drains until a message matches or the deadline passes. Non-matching
// messages are DISCARDED rather than requeued: the periodic telemetry keeps
// arriving through every leg, and a queue this side never drains would make a
// later leg read a message from an earlier one.
func waitMsg(ch <-chan fkipc.Message, d time.Duration,
	pred func(fkipc.Message) bool) (fkipc.Message, bool) {

	deadline := time.After(d)
	for {
		select {
		case m := <-ch:
			if pred(m) {
				return m, true
			}
		case <-deadline:
			return fkipc.Message{}, false
		}
	}
}

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

type gate struct {
	fails int
	step  time.Duration
}

func (g *gate) pass(name, format string, a ...any) {
	fmt.Printf("PASS %s -- %s\n", name, fmt.Sprintf(format, a...))
}

func (g *gate) fail(name, format string, a ...any) {
	g.fails++
	fmt.Printf("FAIL %s -- %s\n", name, fmt.Sprintf(format, a...))
}

// finish is the early exit: a run whose session never came up has nothing left
// to say, and every later leg would fail for the same one reason.
func finish(g *gate, s *fkipc.Session) {
	s.Close()
	fmt.Printf("RESULT failed %d\n", g.fails)
	os.Exit(1)
}
