//go:build tinygo.wasm

// tinygo.wasm, NOT wasm, and the file is not named _wasm.go -- both of which
// were wrong the first time and neither of which fails loudly.
//
// `tinygo info -target=wasm-unknown` reports GOOS=linux GOARCH=arm, so the
// GOARCH-derived `wasm` constraint does not match and a `transport_wasm.go`
// filename would be excluded by the implicit suffix rule as well. Getting it
// wrong compiles: the off-target file matches instead, newTransport returns
// StatusNoTransport, the whole fkapi path is dead-code-eliminated, and the mod
// loads and never speaks. What caught it was the pruning assertion proving ZERO
// event ids where it wanted one.
//
// tinygo.wasm rather than wasm_unknown because wasip1 carries it too, and a
// wasip1 guest wants the same transport.

package fkipc

import (
	"unsafe"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

// The real transport: send_udp, recv_udp, write_file and the version read, all
// through the generated bindings.
//
// THIS FILE IS WHERE THE EVENT ID LIVES, and that is not an accident of
// layout. `fklua mod` prunes the event table (`events` in census.json is how
// many there are) by scanning the wasm for an i32.const reaching fk.subscribe;
// an id it cannot prove constant ships all of them. So the constant has to
// appear AT the fkapi.Subscribe call site and the wrapper has to inline --
// which is a property, not a hope, and internal/guest asserts it against a real
// TinyGo build. It was a live defect on the Rust side once (R6:
// subscribe_filtered lacked #[inline] and shipped 85 KB per load).
//
// WHAT THAT COSTS, ATTRIBUTED, because this comment used to charge it to the
// wrong table: the full event descriptor table is about 55 KB of Lua at the
// 2.1.14 pin. The ~600 KB this once claimed is the MEMBER table's magnitude
// (about a megabyte at the same pin), which is pruned by its own scan and is
// unaffected by whether an event id inlines. R6's 85 KB is a whole-mod delta
// measured downstream on two builds of one real guest, so it is larger than the
// descriptor table alone -- the guest's own filter-building code is in it too.
//
// NOTHING HERE NAMES A NUMBER. The event constant and the member ids come from
// the generated bindings by symbol, so a version bump that renumbers them is
// transparent -- which matters more than usual here, because the RUNTIME id of
// on_udp_packet_received is a different number again (208 on 2.0.77, 212 on
// 2.1.14) and the two namespaces are both correct.
type udpTransport struct {
	port uint16

	// sendFP and recvFP are the for_player arguments, pre-resolved to a
	// pointer-or-nil so the hot path has no branch and no allocation. They
	// differ by profile and they differ from EACH OTHER: a server sends
	// for_player = 0 and pumps with recv_udp(0), a client omits it on both.
	sendFP *uint32
	recvFP *uint32
	fpVal  uint32

	// parts is the {"", frame} LocalisedString, held rather than built.
	//
	// A BARE STRING IS A LOCALE KEY. The probe measured all four forms carrying
	// binary byte-exact, but the bare form was measured on a headless server
	// with nobody to localise FOR; {"", s} is the documented literal-concat
	// form and is literal BY CONSTRUCTION, so it costs one extra dyn_alloc of
	// 32 bytes of ARENA -- released at the binding's own bracket -- and buys
	// not having to think about it again on a client with a locale loaded.
	//
	// Held as a fixed array because fkapi.OfArray takes a variadic, and a
	// variadic allocates a fresh slice on every call: this is a per-send
	// allocation in a heap that is in the save, for two elements that never
	// change shape.
	parts [2]fkapi.Value
}

func newTransport(cfg Config) (Transport, Status) {
	t := &udpTransport{port: cfg.Port}
	t.parts[0] = fkapi.OfString("")
	if cfg.ForPlayer >= 0 {
		t.fpVal = uint32(cfg.ForPlayer)
		t.sendFP = &t.fpVal
	}
	// The RECEIVE side is not the send side's mirror. ProfileClient pumps with
	// a bare recv_udp(), which is what every graphical-client mod in the
	// ecosystem does; ProfileServer pumps with recv_udp(0), which is the arm
	// the probe verified working on 2.1.14 -- and the arm that kills 2.0.77,
	// which is why Pump asks the version gate first.
	if cfg.Profile != ProfileClient {
		t.recvFP = &t.fpVal
	}
	// The status is deliberately not propagated. The only way this fails is a
	// guest that exports no fk_on_event, which is an authoring mistake rather
	// than a runtime condition -- and refusing Open over it would take the
	// OUTBOUND half down too, which works on every version and is the direction
	// that is free. fk_mod.lua logs the refusal, and internal/guest's
	// end-to-end test is what actually catches it.
	fkapi.Subscribe(fkapi.EventOnUdpPacketReceived)
	return t, StatusOK
}

// Send returns NOTHING, and on this arm that is the whole point: this is the
// one implementation that runs inside a lockstep game, so this is where a
// return value would become a word in storage.fk_mem that differs between a
// server started with --enable-lua-udp and a client that was not. There is no
// value to return and therefore no branch to write. See the seam's own comment
// in transport.go.
func (t *udpTransport) Send(frame []byte) {
	if len(frame) == 0 {
		return
	}
	// unsafe.String rather than string(frame): the host copies the bytes before
	// send_udp returns and the buffer is not touched during the call, so a Go
	// copy here would be a per-frame allocation of up to MaxFrameCeiling bytes
	// in a heap that is in the save.
	t.parts[1] = fkapi.OfString(unsafe.String(&frame[0], len(frame)))
	v := fkapi.Value{Tag: fkapi.TagArray, Array: t.parts[:]}
	// The error is DROPPED HERE rather than carried one frame further. Its
	// value is a fact about how this peer was launched, which is exactly the
	// class of fact guest state may not hold.
	_ = fkapi.Helpers.SendUdp(t.port, v, t.sendFP)
}

// Poll calls recv_udp, and the datagrams do NOT come back through deliver: the
// engine dispatches them as on_udp_packet_received events inside this call,
// which reach the link through Event below. It always reports false, so Pump's
// drain loop runs exactly once -- which is the measured shape, one call
// draining a 20-packet backlog within the tick, in order, complete.
func (t *udpTransport) Poll(deliver func(uint16, []byte)) bool {
	fkapi.Helpers.RecvUdp(t.recvFP)
	return false
}

func (t *udpTransport) Event(id, ptr uint32, deliver func(uint16, []byte)) bool {
	if id != fkapi.EventOnUdpPacketReceived {
		return false
	}
	ev := fkapi.ReadOnUdpPacketReceived(ptr)
	if len(ev.Payload) == 0 {
		return true
	}
	// SourcePort IS FORWARDED RATHER THAN FILTERED ON HERE. --enable-lua-udp
	// binds one socket for the whole game, so this event fires in EVERY mod for
	// EVERY mod's datagrams; the sender's port is what tells them apart, and
	// the decision belongs to the link, which owns the peer port and the
	// counters. See Link.deliver.
	//
	// The payload string points into the host's scratch region (or, above it,
	// into arena memory the outermost dispatch will release). Reading it as
	// bytes without a copy is exactly the lifetime the handlers document, and
	// the copy the application owes happens where the application decides.
	deliver(ev.SourcePort, unsafe.Slice(unsafe.StringData(ev.Payload), len(ev.Payload)))
	return true
}

// WriteFile returns NOTHING, for Send's reason. See transport.go.
func (t *udpTransport) WriteFile(name string, data []byte) {
	var s string
	if len(data) > 0 {
		s = unsafe.String(&data[0], len(data))
	}
	appendMode := false
	_ = fkapi.Helpers.WriteFile(name, fkapi.OfString(s), &appendMode, t.sendFP)
}

// BaseVersion reads helpers.game_version, ONCE, and it is the cheapest bound
// surface that answers the question: one host call, one short string, no
// container -- against script.active_mods, which materialises every mod's name
// and version into the guest heap to find one of them.
//
// It is also available where this has to run. Open is called from init(), i.e.
// from control.lua's main chunk, where `game` does not exist yet; `helpers`
// does, and game_version carries no stage restriction.
//
// Deterministic on every peer by construction: a multiplayer game requires
// identical builds.
func (t *udpTransport) BaseVersion() (Version, bool) {
	s, err := fkapi.Helpers.GameVersion()
	if err != nil {
		return Version{}, false
	}
	return ParseVersion(s)
}

// Log goes to the game log, which is not CRC'd -- the only sanctioned sink for
// a per-peer fact. See the seam's comment in transport.go.
func (t *udpTransport) Log(msg string) { fk.Log(msg) }
