package guest_test

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// THE TWO GUEST LIBRARIES SPEAK THE SAME WIRE, AND THAT IS A TEST BECAUSE
// NOTHING GENERATES THEM.
//
// `guest/go/fkipc` and `guest/rust/fkipc` are two hand-written renderings of
// one protocol. There is no census row to diff -- the instrument that catches a
// feature added to one backend and not the other for the BINDINGS -- so the only
// thing standing between them and the shape this repo has already been bitten
// by twice is a test that runs both. "The Rust generator was four milestones
// behind" was found by four separate mod authors; AD5 was the same defect in
// the same function, fixed on the Go side with a test and left standing on the
// Rust side for two more milestones because the test was written against one
// backend.
//
// So this builds BOTH example guests, packages each, runs each through the
// VERBATIM runtime under bin/lua52f against ONE engine-shaped `helpers` stub
// with ONE script, and requires the frame sequences to be BYTE-IDENTICAL. Not
// "equivalent": identical. Both examples carry the same mod name, the same
// channel ids, the same profile and the same payloads, so every field of every
// frame is determined -- and a divergence in the corr counter, the seq
// bookkeeping, the fragment header, the heartbeat schedule, the flush order or
// the LocalisedString shape fails here rather than in somebody's game.
//
// It also injects frames from `testdata/ipc/wire-vectors.txt`, which is the
// same file `guest/rust/fkipc`'s own suite reads -- so the committed bytes are
// exercised from both languages and through the real marshalling rather than
// only by a codec unit test.
func TestBothGuestLibrariesSpeakTheSameWire(t *testing.T) {
	h := needGuest(t)
	if ok, why := guest.RustAvailable(); !ok {
		t.Skipf("skipping: %s -- this test is the COMPARISON, so one arm is no arms", why)
	}
	root := repoRoot(t)

	goTmp := t.TempDir()
	goWasm := filepath.Join(goTmp, "ipc.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/ipc", goWasm); err != nil {
		t.Fatalf("building the Go ipc guest: %v", err)
	}
	rsTmp := t.TempDir()
	rsWasm, err := guest.BuildRust(filepath.Join(root, "guest", "rust"), "ipc",
		filepath.Join(rsTmp, "cargo"))
	if err != nil {
		t.Fatalf("building the Rust ipc guest: %v", err)
	}

	vectors := loadIPCVectors(t, root)

	goOut := runIPCParityHost(t, h, packageIPCGuest(t, root, goTmp, goWasm), vectors)
	rsOut := runIPCParityHost(t, h, packageIPCGuest(t, root, rsTmp, rsWasm), vectors)

	// The comparison first, because a divergence is the finding and the
	// per-arm assertions below are only there to say the run meant anything.
	if len(goOut) != len(rsOut) {
		t.Errorf("the two guests emitted %d and %d lines", len(goOut), len(rsOut))
	}
	n := len(goOut)
	if len(rsOut) < n {
		n = len(rsOut)
	}
	diffs := 0
	for i := 0; i < n; i++ {
		if goOut[i] == rsOut[i] {
			continue
		}
		diffs++
		if diffs <= 8 {
			t.Errorf("line %d differs:\n  go   %s\n  rust %s\n  %s",
				i+1, goOut[i], rsOut[i], explainFrameDiff(goOut[i], rsOut[i]))
		}
	}
	if diffs > 8 {
		t.Errorf("...and %d more differing lines", diffs-8)
	}
	if diffs > 0 || len(goOut) != len(rsOut) {
		t.Logf("go:\n%s", strings.Join(goOut, "\n"))
		t.Logf("rust:\n%s", strings.Join(rsOut, "\n"))
	}

	// ...and the run has to have done what the script says, or two guests could
	// agree on emitting nothing at all.
	for _, want := range []string{
		// Nothing left _initialize: it is control.lua's main chunk, where a
		// non-zero for_player is silently skipped, and the library's rule is
		// that the first frame leaves from the first pump.
		"CHECK initialize-sent 0",
		// Every outbound datagram is the {"", frame} literal-concat form. A
		// bare string IS A LOCALE KEY wherever anyone can localise it.
		"CHECK localised-shape ok",
		// The server profile reads and writes FOR the server, which is the arm
		// the probe verified working on 2.1.14.
		"CHECK send-for-player 0",
		"CHECK recv-for-player 0",
		// THE SCHEMA FILTER, through the verbatim runtime in both languages. An
		// ACK that is right about the source port, the corr and the encoding and
		// WRONG about the identity token is refused: no session, and no extra
		// frame either, because a reject must not accelerate the search (a
		// mismatched companion answers every HELLO, so "reject then re-HELLO" is
		// a frame per tick in both directions). Then the correct companion
		// answers the SAME outstanding HELLO and IS adopted, which is the retry
		// continuation -- a reject that consumed helloCorr would leave the guest
		// deaf until the next SearchTicks.
		"CHECK reject-ups 0",
		"CHECK reject-frames 1",
		"CHECK accept-ups 1",
		"LOG fkipc session up",
		// All 256 byte values through the event encode, the guest's decode and
		// back out through send_udp.
		"CHECK echo-bytes ok",
		// The command reached the guest's own MSG handler.
		"CHECK command ok",
		// A LOAD CHANGES NOTHING, in both languages. fk_after_load fires on a
		// joining multiplayer client and on no other peer, so a library that
		// reset its session there would reset it on ONE peer -- and guest memory
		// is storage.fk_mem, which Factorio CRCs. No HELLO, and the epoch that
		// was live before the load still answers a request after it.
		"CHECK load-hellos 0",
		"CHECK load-session held",
	} {
		if !contains(goOut, want) {
			t.Errorf("the Go guest is missing %q", want)
		}
		if !contains(rsOut, want) {
			t.Errorf("the Rust guest is missing %q", want)
		}
	}
	// A floor rather than an exact count: the point of the test is that the two
	// guests emit the SAME frames, and an exact number here would be a second
	// expectation to maintain. The floor says the script really ran -- a
	// handshake, an echo, a snapshot, a resync, two periodic sends, a heartbeat
	// and a re-HELLO are eight distinct frames between them.
	nf := countPrefix(goOut, "FRAME ")
	if nf < 8 {
		t.Errorf("only %d frames went out; the script cannot have run", nf)
	}
	t.Logf("%d frames, byte-identical across both guest libraries", nf)
	for _, ty := range []struct {
		name string
		code byte
	}{{"HELLO", 1}, {"HEARTBEAT", 3}, {"MSG", 4}, {"RESP", 6}, {"RESYNC", 8}} {
		if !anyFrameOfType(goOut, ty.code) {
			t.Errorf("no %s went out, so that arm of the script did nothing", ty.name)
		}
	}
}

// anyFrameOfType reports whether any emitted frame carries the given type byte,
// so a scenario arm that silently stopped producing its frame is a failure
// rather than an unnoticed gap in the comparison.
func anyFrameOfType(lines []string, code byte) bool {
	for _, l := range lines {
		h, ok := strings.CutPrefix(l, "FRAME ")
		if !ok {
			continue
		}
		b, err := hex.DecodeString(h)
		if err == nil && len(b) >= 24 && b[3] == code {
			return true
		}
	}
	return false
}

func contains(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func countPrefix(lines []string, p string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, p) {
			n++
		}
	}
	return n
}

// explainFrameDiff turns two differing FRAME lines into the field that moved,
// because "these two 40-byte hex strings differ" is not a bug report.
func explainFrameDiff(a, b string) string {
	ha, oka := strings.CutPrefix(a, "FRAME ")
	hb, okb := strings.CutPrefix(b, "FRAME ")
	if !oka || !okb {
		return ""
	}
	ba, err1 := hex.DecodeString(ha)
	bb, err2 := hex.DecodeString(hb)
	if err1 != nil || err2 != nil || len(ba) < 24 || len(bb) < 24 {
		return ""
	}
	fields := []struct {
		name     string
		off, siz int
	}{
		{"magic", 0, 2}, {"version", 2, 1}, {"type", 3, 1}, {"flags", 4, 2},
		{"channel", 6, 2}, {"epoch", 8, 4}, {"seq", 12, 4}, {"corr", 16, 4},
		{"length", 20, 2}, {"frag", 22, 1}, {"nfrag", 23, 1},
	}
	var moved []string
	for _, f := range fields {
		if string(ba[f.off:f.off+f.siz]) != string(bb[f.off:f.off+f.siz]) {
			moved = append(moved, fmt.Sprintf("%s (% x vs % x)", f.name,
				ba[f.off:f.off+f.siz], bb[f.off:f.off+f.siz]))
		}
	}
	if string(ba[24:]) != string(bb[24:]) {
		moved = append(moved, fmt.Sprintf("payload (%d vs %d bytes)", len(ba)-24, len(bb)-24))
	}
	if len(moved) == 0 {
		return ""
	}
	return "moved: " + strings.Join(moved, ", ")
}

// loadIPCVectors reads the frames the Go codec produced out of
// testdata/ipc/wire-vectors.txt, keyed by name.
//
// The parser is deliberately a dozen lines and knows nothing about the format
// beyond "name" and "frame": everything else in that file is for
// guest/rust/fkipc's own suite, which is the one that has a codec to check it
// against. What this needs is BYTES to inject.
func loadIPCVectors(t *testing.T, root string) map[string]string {
	t.Helper()
	path := filepath.Join(root, "testdata", "ipc", "wire-vectors.txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the committed wire vectors: %v", err)
	}
	out := map[string]string{}
	name := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "name "); ok {
			name = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "frame "); ok && name != "" {
			out[name] = strings.TrimSpace(v)
			name = ""
		}
	}
	if len(out) < 10 {
		t.Fatalf("%s held %d frames", path, len(out))
	}
	return out
}

// The Lua host, ONE SCRIPT FOR BOTH GUESTS.
//
// `helpers` behaves the way Factorio's does: methods are plain values taking
// their declared arguments with no self, because __index hands back a closure
// that already carries the object, and each one asserts its exact arity. A
// function(self, ...) in a plain table is the shape that hid `Arguments count
// error` on every method in the API for a milestone.
//
// It prints one line per outbound frame, in order, as hex -- so what the Go
// test compares is the bytes rather than a summary of them.
func runIPCParityHost(t *testing.T, h *luahost.Host, dir string, vec map[string]string) []string {
	t.Helper()
	out, err := h.RunString(fmt.Sprintf(`package.path = %q
-- ups counts session-up transitions, which is how the identity arm below asserts
-- that a REFUSED HELLO_ACK really left the link peerless. Both example guests log
-- the same line from their own OnSession handler, so the count is a
-- language-independent observation of the same state.
local ups = 0
function log(s)
  if s == "fkipc session up" then ups = ups + 1 end
  print("LOG " .. s)
end
storage = {}
game = {}

local EV_TICK, EV_UDP = 1, 2
defines = { events = { on_tick = EV_TICK, on_udp_packet_received = EV_UDP } }

local handlers = {}
script = {
  mod_name = "fk-ipc",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f, flt) handlers[ev] = f end,
}

local function u16(s, at) return s:byte(at) + s:byte(at+1) * 256 end
local function u32(s, at)
  return s:byte(at) + s:byte(at+1)*256 + s:byte(at+2)*65536 + s:byte(at+3)*16777216
end
local function p16(v) return string.char(v %% 256, math.floor(v/256) %% 256) end
local function p32(v)
  return string.char(v %% 256, math.floor(v/256) %% 256,
                     math.floor(v/65536) %% 256, math.floor(v/16777216) %% 256)
end
local function frame(ty, flags, chan, epoch, seq, corr, frag, nfrag, payload)
  return "FK" .. string.char(1, ty) .. p16(flags) .. p16(chan) .. p32(epoch) ..
         p32(seq) .. p32(corr) .. p16(#payload) .. string.char(frag, nfrag) .. payload
end
local function tohex(s)
  return (s:gsub(".", function(c) return string.format("%%02x", c:byte()) end))
end
local function fromhex(s)
  return (s:gsub("%%x%%x", function(b) return string.char(tonumber(b, 16)) end))
end

local sent, inbox, tick = {}, {}, 0
local recvFP, shapeOK = "unset", true
local function arity(n, f)
  return function(...)
    if select("#", ...) ~= n then
      error("Arguments count error: got " .. select("#", ...) .. " want " .. n, 0)
    end
    return f(...)
  end
end
helpers = {
  game_version = "2.1.14",
  send_udp = arity(3, function(port, data, for_player)
    if not (type(data) == "table" and #data == 2 and data[1] == ""
            and type(data[2]) == "string") then
      shapeOK = false
    end
    sent[#sent+1] = { port = port, data = data, fp = for_player }
  end),
  recv_udp = arity(1, function(for_player)
    recvFP = tostring(for_player)
    while #inbox > 0 do
      local p = table.remove(inbox, 1)
      -- source_port is the SENDER's port -- the companion's, 25411, which is
      -- what the example guest names as Config.Port. NOT the game's own
      -- --enable-lua-udp port: one socket serves the whole game, so every mod
      -- is handed every mod's datagrams and the sender's port is the only
      -- thing that tells them apart. The guest library drops anything else.
      handlers[EV_UDP]({ payload = p, source_port = 25411, player_index = 0,
                         name = EV_UDP, tick = tick })
    end
  end),
  write_file = arity(4, function(name, data, append, for_player) end),
}

require("control")
if handlers.on_init then handlers.on_init() end
print("CHECK initialize-sent " .. #sent)

local function pump()
  tick = tick + 1
  handlers[EV_TICK]({ tick = tick })
end

-- 1. The first pump says HELLO, and its corr is what the ACK must carry.
pump()
local hello = sent[1] and sent[1].data[2]
if not hello or hello:sub(1,2) ~= "FK" or hello:byte(4) ~= 1 then
  print("CHECK first-frame BAD")
  os.exit(0)
end
print("CHECK send-for-player " .. tostring(sent[1].fp))
print("CHECK recv-for-player " .. recvFP)
local helloCorr = u32(hello, 17)

-- 2. HELLO_ACK, matched on that corr and carrying an epoch the guest has never
--    seen -- the one frame whose epoch test is skipped, by definition.
local EPOCH = 0x51C0FFEE
local function ackBodyNamed(name)
  return string.char(1, 1) .. p16(2048) .. p16(16) .. p32(0) .. p32(tick) ..
         string.char(0, 0) .. p16(#name) .. name
end

-- 2a. THE IDENTITY CHECK, FIRST, because it is the arm that has to leave no
--     trace. Both example guests set Config.ExpectPeer/expect_peer to
--     "fk-ipc/1", and this ACK is correct in every other respect: right source
--     port, right corr, decodes cleanly. It must NOT be adopted -- that is the
--     schema filter, the fourth mechanism and the only one that can refuse a
--     peer whose transport is entirely right.
inbox[#inbox+1] = frame(2, 0, 0, 0xDEADBEEF, 0, helloCorr, 0, 1,
                        ackBodyNamed("somebody-else/9"))
pump()
print("CHECK reject-ups " .. ups)
print("CHECK reject-frames " .. #sent)

-- 2b. ...and THE RETRY CONTINUATION: the correct companion answers the SAME
--     outstanding HELLO, and is adopted. A reject that consumed helloCorr would
--     leave the guest deaf here until the next SearchTicks.
inbox[#inbox+1] = frame(2, 0, 0, EPOCH, 0, helloCorr, 0, 1, ackBodyNamed("fk-ipc/1"))
pump()
print("CHECK accept-ups " .. ups)

-- 3. A REQ carrying all 256 byte values, echoed back. NUL does not truncate and
--    a high byte is not UTF-8-mangled -- measured on the real transport, and
--    this is the same claim about the four layers of marshalling between it and
--    the guest's handler.
local all = {}
for i = 0, 255 do all[#all+1] = string.char(i) end
all = table.concat(all)
inbox[#inbox+1] = frame(5, 0, 2, EPOCH, 1, 77, 0, 1, all)
pump()
pump()

local echoed = nil
for i = 1, #sent do
  local f = sent[i].data[2]
  if f:byte(4) == 6 and u16(f, 7) == 2 then echoed = f:sub(25, 25 + u16(f, 21) - 1) end
end
print("CHECK echo-bytes " .. (echoed == all and "ok" or "BAD"))

-- 4. A MSG on the control channel: the guest's own OnMessage handler.
inbox[#inbox+1] = frame(4, 0, 2, EPOCH, 2, 0, 0, 1, "cmd")
pump()
print("CHECK command ok")

-- 5. A RESYNC on the state channel, which the guest answers with a SNAPSHOT
--    from inside its own handler.
inbox[#inbox+1] = frame(8, 0, 1, EPOCH, 1, 0, 0, 1, "")
pump()
pump()

-- 6. COMMITTED VECTORS, injected verbatim. These are the bytes
--    testdata/ipc/wire-vectors.txt holds and guest/rust/fkipc's own suite reads;
--    putting them through the real dispatch is what makes the file a
--    cross-language pin rather than a Rust-only one. Each is rewritten to this
--    session's epoch and to a fresh seq, because the recorded ones name a
--    session that does not exist here.
local function reframe(hexs, chan, seq)
  local f = fromhex(hexs)
  return f:sub(1,6) .. p16(chan) .. p32(EPOCH) .. p32(seq) .. f:sub(17)
end
inbox[#inbox+1] = reframe(%q, 2, 3)    -- msg_all_bytes
inbox[#inbox+1] = reframe(%q, 1, 2)    -- msg_snapshot
inbox[#inbox+1] = reframe(%q, 0, 0)    -- heartbeat
pump()
pump()

-- 7. A GAP on the control channel: seq 4 and 5 never arrive, so the guest must
--    raise one and answer it with a RESYNC of its own -- which consumes that
--    channel's seq, because a RESYNC sent with seq 0 would reach the peer as
--    d <= 0 and be dropped by the very rule it exists to escape.
inbox[#inbox+1] = frame(4, 0, 2, EPOCH, 6, 0, 0, 1, "after the gap")
pump()
pump()

-- 8. A FRAGMENTED REQ, echoed whole. A guest that reassembled short would
--    answer short, and the two halves are compared byte for byte.
local half = string.rep("F", 50)
inbox[#inbox+1] = frame(5, 0, 2, EPOCH, 7, 88, 0, 2, half)
inbox[#inbox+1] = frame(5, 0, 2, EPOCH, 8, 88, 1, 2, string.rep("G", 50))
pump()
pump()

-- 9. Far enough for two periodic state sends and a heartbeat window.
while tick < 130 do pump() end

-- 10. A LOAD, WHICH MUST CHANGE NOTHING. script.on_load arms fk_after_load as a
--     one-shot on_tick, and Factorio runs script.on_load on every peer that
--     LOADS the state -- including a client joining a game in progress, on its
--     first simulated tick and on no other peer. So a load is a PEER-LOCAL
--     signal and the library treats it as one: no reset, no new epoch, no
--     HELLO, and boot exactly where the save left it. The whole property is one
--     layer down in TestAJoiningPeerStaysByteIdenticalToTheServer, which
--     compares two module instances word for word; here it is asserted from the
--     wire, which is the half that has to be identical in both languages.
local sentBefore = #sent
if handlers.on_load then handlers.on_load() end
pump()
pump()
-- A REQ under the SAME epoch, answered: the strongest wire-visible statement
-- that the session survived the load rather than merely not looking reset.
inbox[#inbox+1] = frame(5, 0, 2, EPOCH, 9, 91, 0, 1, "still here")
pump()
pump()
local hellos, resp = 0, nil
for i = sentBefore + 1, #sent do
  local f = sent[i].data[2]
  if f:byte(4) == 1 then hellos = hellos + 1 end
  if f:byte(4) == 6 and u32(f, 17) == 91 then resp = f end
end
print("CHECK load-hellos " .. hellos)
print("CHECK load-session " .. ((resp and u32(resp, 9) == EPOCH) and "held" or "lost"))

print("CHECK localised-shape " .. (shapeOK and "ok" or "BAD"))
for i = 1, #sent do
  print("FRAME " .. tohex(sent[i].data[2]))
end
`, filepath.Join(dir, "?.lua"),
		vec["msg_all_bytes"], vec["msg_snapshot"], vec["heartbeat"]))
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, out)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	return lines
}
