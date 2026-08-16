package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Techrocket9/fklua/sdk/go/fkipc"
)

// The two channels every demo mod uses. They are split because a channel's seq
// is shared by everything on it, so a lost REQ on a mixed channel would raise a
// gap -- and therefore a RESYNC and a snapshot -- on whatever telemetry shared
// it.
const (
	chanTelemetry = 1
	chanControl   = 2
)

// sliderSpec describes one control. The UI is GENERATED FROM THESE rather than
// hand-written per mod: two cards written by hand is two places to forget a
// unit, and the smoke run drives the same list, so an added slider is
// automatically both rendered and exercised.
type sliderSpec struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	Min   int     `json:"min"`
	Max   int     `json:"max"`
	Step  int     `json:"step"`
	Init  int     `json:"init"`
	Scale float64 `json:"scale"` // display value = wire value * scale
	Unit  string  `json:"unit"`
	// Smoke is the value the headless run sets, and it must differ from Init or
	// the leg proves nothing: a readback that matches the guest's own starting
	// value is a readback that would pass with the RPC deleted.
	Smoke int `json:"-"`
}

// readoutSpec is one telemetry field the card displays.
type readoutSpec struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	Scale float64 `json:"scale"`
	Unit  string  `json:"unit"`
	// Digits after the decimal point once Scale is applied.
	Digits int `json:"digits"`
}

// modSpec is one mod: its port, its identity, its sliders, its readouts.
type modSpec struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Lang  string `json:"lang"`
	Blurb string `json:"blurb"`
	Port  uint16 `json:"port"`
	// Identity is the pairing's build-time token, and it is COMPILED INTO THE
	// GUEST as well -- guest/go/examples/demo-daylight and
	// guest/rust/examples/demo-circle name the same two strings. One token names
	// the CONTRACT rather than either party, so it is both this side's Name and
	// its ExpectedName.
	//
	// It is what makes a crossed -daylight-port/-circle-port a session that
	// never comes up instead of a slider that silently drives the wrong mod: the
	// port filter answers "is this frame from the process I was pointed at", and
	// only the token answers "is that process the one I was BUILT against".
	Identity string        `json:"identity"`
	Sliders  []sliderSpec  `json:"sliders"`
	Readouts []readoutSpec `json:"readouts"`
}

func specs(daylightPort, circlePort uint16) []modSpec {
	return []modSpec{{
		Key:   "daylight",
		Title: "demo-daylight",
		Lang:  "Go / TinyGo",
		Blurb: "Drags the sun. The surface's day/night cycle is frozen so the " +
			"slider sticks; the readout is what the surface actually has, not " +
			"what was asked for.",
		Port:     daylightPort,
		Identity: "fk-demo-daylight/1",
		Sliders: []sliderSpec{{
			Key: "daytime", Label: "Daytime", Min: 0, Max: 1000, Step: 5,
			Init: 500, Scale: 0.001, Unit: "", Smoke: 250,
		}},
		Readouts: []readoutSpec{
			{Key: "daytime", Label: "daytime", Scale: 0.001, Digits: 3},
			{Key: "frozen", Label: "frozen", Scale: 1, Digits: 0},
			{Key: "player", Label: "player", Scale: 1, Digits: 0},
			{Key: "px", Label: "player x", Scale: 0.01, Digits: 2},
			{Key: "py", Label: "player y", Scale: 0.01, Digits: 2},
		},
	}, {
		Key:   "circle",
		Title: "demo-circle",
		Lang:  "Rust",
		Blurb: "Resizes and recolours a rendered circle at spawn, and counts " +
			"what is inside it. Evolution is the enemy force's, in parts per " +
			"million.",
		Port:     circlePort,
		Identity: "fk-demo-circle/1",
		Sliders: []sliderSpec{{
			Key: "radius", Label: "Radius", Min: 2, Max: 60, Step: 1,
			Init: 12, Scale: 1, Unit: " tiles", Smoke: 30,
		}, {
			Key: "hue", Label: "Hue", Min: 0, Max: 359, Step: 1,
			Init: 40, Scale: 1, Unit: "°", Smoke: 200,
		}},
		Readouts: []readoutSpec{
			{Key: "radius", Label: "radius", Scale: 1, Digits: 0},
			{Key: "hue", Label: "hue", Scale: 1, Digits: 0},
			{Key: "evo", Label: "evolution", Scale: 0.0001, Digits: 4},
			{Key: "entities", Label: "entities inside", Scale: 1, Digits: 0},
		},
	}}
}

// modState is what the UI shows for one mod. Everything in it is JSON-tagged
// because it goes out over SSE verbatim.
type modState struct {
	Key       string         `json:"key"`
	Up        bool           `json:"up"`
	Epoch     string         `json:"epoch"`
	Sessions  uint32         `json:"sessions"`
	GuestTick uint32         `json:"guestTick"`
	Frames    uint32         `json:"frames"`
	Telemetry map[string]int `json:"telemetry"`
	Raw       string         `json:"raw"`
	LastAck   string         `json:"lastAck"`
	AckMillis int64          `json:"ackMillis"`
	Age       int64          `json:"ageMillis"`
}

// link is one mod: its session, its spec, and the last thing it said.
type link struct {
	spec modSpec
	sess *fkipc.Session
	log  *slog.Logger

	// What it takes to dial again with a different identity, which is the whole
	// of what the smoke run's identity leg needs: a socket bound to this mod's
	// port cannot be shared, so proving a mismatch in a LIVE game means closing
	// this session and opening another on the same port.
	gamePort uint16
	notify   func()

	mu        sync.Mutex
	telemetry map[string]int
	raw       string
	seenAt    time.Time
	lastAck   string
	ackTook   time.Duration
	frames    uint32
	// history is every telemetry payload this session has seen, kept only for
	// the smoke run's isolation leg -- a UI has no use for it and a long
	// graphical session should not grow one, so it is capped.
	history []string
	keepAll bool
	// events is every session transition this link has seen, which is how the
	// identity leg asserts that SessionRejected really was raised rather than
	// inferring it from a session that merely never came up.
	events []fkipc.SessionEvent
}

func newLink(spec modSpec, gamePort uint16, lg *slog.Logger, keepAll bool,
	notify func()) (*link, error) {

	l := &link{spec: spec, log: lg, telemetry: map[string]int{}, keepAll: keepAll,
		gamePort: gamePort, notify: notify}
	// ONE TOKEN NAMES THE CONTRACT, so this side's own identity and what it
	// requires of the guest are the same string. See modSpec.Identity.
	if err := l.dial(spec.Identity, spec.Identity); err != nil {
		return nil, err
	}
	return l, nil
}

// dial opens a session on this mod's port under a stated identity and wires the
// handlers onto it. Everything that survives a redial -- the telemetry snapshot,
// the frame count, the history -- lives on the link rather than on the session.
func (l *link) dial(name, expected string) error {
	s, err := fkipc.Dial(fkipc.Options{
		GamePort:     l.gamePort,
		ListenPort:   l.spec.Port,
		Name:         name,
		ExpectedName: expected,
		Logger:       l.log,
	})
	if err != nil {
		return err
	}
	l.sess = s

	s.OnSession(func(ev fkipc.SessionEvent, epoch uint32) {
		// The binary's own transition log, which is what the harness reads when
		// something goes wrong at three in the morning.
		l.log.Info("session", "mod", l.spec.Key, "state", ev.String(),
			"epoch", "0x"+strconv.FormatUint(uint64(epoch), 16))
		l.mu.Lock()
		l.events = append(l.events, ev)
		l.mu.Unlock()
		l.notify()
	})
	s.Subscribe(chanTelemetry, func(m fkipc.Message) {
		l.observe(string(m.Payload))
		l.notify()
	})
	return nil
}

// redial closes this link's session and opens another on the same port under a
// different identity.
//
// It exists for the smoke run's identity leg and for nothing else. A companion
// binds ONE socket per mod port, so there is no way to hold a correctly-paired
// session and a deliberately mismatched one at the same time -- which is why
// that leg runs last and restores the correct pairing when it is done.
func (l *link) redial(name, expected string) error {
	if l.sess != nil {
		l.sess.Close()
	}
	l.mu.Lock()
	l.events = nil
	l.mu.Unlock()
	return l.dial(name, expected)
}

// sessionEvents is a snapshot of the transitions seen since the last redial.
func (l *link) sessionEvents() []fkipc.SessionEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]fkipc.SessionEvent(nil), l.events...)
}

// observe parses one telemetry frame.
//
// THE PAYLOAD IS `k=v` PAIRS OF DECIMAL INTEGERS and that is a protocol
// decision made on the guest's side: formatting an f64 in a guest means either
// reflection or a hand-written dtoa two implementations would have to agree on
// digit for digit, in a heap that is in the save. The scaling back to a real
// number happens here, where there is a printf.
//
// A key the guest stopped sending is REMOVED rather than left at its last
// value: demo-daylight omits px/py entirely when nobody is in the game, and a
// stale position frozen on screen would read as a player standing still.
func (l *link) observe(payload string) {
	next := map[string]int{}
	for _, f := range strings.Fields(payload) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		next[k] = n
	}
	l.mu.Lock()
	l.telemetry = next
	l.raw = payload
	l.seenAt = time.Now()
	l.frames++
	if l.keepAll {
		l.history = append(l.history, payload)
	}
	l.mu.Unlock()
}

func (l *link) state() modState {
	st := l.sess.Stats()
	l.mu.Lock()
	defer l.mu.Unlock()
	age := int64(-1)
	if !l.seenAt.IsZero() {
		age = time.Since(l.seenAt).Milliseconds()
	}
	return modState{
		Key:       l.spec.Key,
		Up:        st.Up,
		Epoch:     "0x" + strconv.FormatUint(uint64(st.Epoch), 16),
		Sessions:  st.Sessions,
		GuestTick: st.GuestTick,
		Frames:    l.frames,
		Telemetry: l.telemetry,
		Raw:       l.raw,
		LastAck:   l.lastAck,
		AckMillis: l.ackTook.Milliseconds(),
		Age:       age,
	}
}

func (l *link) snapshotHistory() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.history...)
}

// set is one slider move.
//
// A SET IS A SET, so this is idempotent by construction -- which is what the
// protocol asks of every request, because a retried REQ can be executed again
// once it falls outside the dedup window. The ack carries the value the guest
// ACTUALLY APPLIED after its own clamping, which is what the UI displays: a UI
// echoing its own request would be showing itself.
func (l *link) set(ctx context.Context, key string, value int) (string, error) {
	req := "set " + key + " " + strconv.Itoa(value)
	start := time.Now()
	out, err := l.ask(ctx, []byte(req))
	took := time.Since(start)
	if err != nil {
		return "", err
	}
	ack := string(out)
	l.mu.Lock()
	l.lastAck, l.ackTook = ack, took
	l.mu.Unlock()
	return ack, nil
}

// ask is Request plus the ONE recovery the protocol asks an application to
// perform.
//
// ErrSessionLost is not "the request failed", it is "THE OUTCOME IS UNKNOWN" --
// only the application knows whether re-asking is safe, which is why the
// library never retries across a session boundary by itself. It fires for real
// here rather than being defensive: starting a headless server LOADS the map,
// so the guest reloads on its first tick, and fkipc's first-tick window
// (agents/ipc.md, P6) puts two HELLOs on the wire one tick apart. A companion
// listening for both mints two sessions and anything in flight dies exactly
// here. A set is idempotent, so this re-asks once.
func (l *link) ask(ctx context.Context, payload []byte) ([]byte, error) {
	for try := 0; ; try++ {
		rctx, cancel := context.WithTimeout(ctx, requestTimeout)
		out, err := l.sess.Request(rctx, chanControl, payload)
		cancel()
		recoverable := errors.Is(err, fkipc.ErrSessionLost) ||
			errors.Is(err, fkipc.ErrNoSession) ||
			errors.Is(err, fkipc.ErrPeerQuiet)
		if try > 0 || !recoverable {
			return out, err
		}
		l.log.Info("re-asking across a session boundary", "mod", l.spec.Key, "err", err)
		deadline := time.Now().Add(requestTimeout)
		for !l.sess.Stats().Up && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func (l *link) waitUp(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if l.sess.Stats().Up {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// waitTelemetry blocks until a telemetry frame satisfies pred, or the deadline
// passes. It polls the parsed snapshot rather than tapping the message stream
// so that the UI's own subscriber and this share one source of truth.
func (l *link) waitTelemetry(d time.Duration, pred func(map[string]int) bool) (map[string]int, bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		t := l.telemetry
		l.mu.Unlock()
		if len(t) > 0 && pred(t) {
			return t, true
		}
		time.Sleep(25 * time.Millisecond)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.telemetry, false
}

func (l *link) marshalState() []byte {
	b, _ := json.Marshal(l.state())
	return b
}
