package guest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// FKIPC END TO END, THROUGH THE REAL control.lua.
//
// The conformance suite in sdk/go drives the same guest state machine against
// the real SDK, so the protocol is not what is under test here. What IS under
// test is the one file that suite cannot reach: guest/go/fkipc's wasm-only
// transport, and the four seams on either side of it that are each separately
// correct and can still be wrong together --
//
//   - Open runs from a package initialiser, inside _initialize, and must SEND
//     NOTHING there;
//   - send_udp's data is a LocalisedString, and the library must put {"", frame}
//     on it. A bare string IS A LOCALE KEY wherever anyone can localise it, so
//     this is a correctness property and not a style;
//   - an inbound datagram arrives as an on_udp_packet_received EVENT raised
//     from inside recv_udp, is encoded into the event scratch buffer by
//     write_struct, and is read back by the generated decoder -- four pieces of
//     marshalling between the wire and the guest's handler;
//   - the bytes survive all of it, including NUL and every high byte, which is
//     what the probe measured on the real transport in both directions.
//
// The `helpers` stub is FACTORIO'S SHAPE and not a convenience: methods are
// plain values taking their declared arguments with no self, because __index
// hands back a closure that already carries the object, and each one asserts
// its exact arity. A function(self, ...) in a plain table is the shape that hid
// `Arguments count error` on every method in the API for a milestone.
func TestAnFkipcGuestSpeaksTheWireThroughTheRealRuntime(t *testing.T) {
	h := needGuest(t)
	root, tmp := repoRoot(t), t.TempDir()
	out := filepath.Join(tmp, "ipc.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/ipc", out); err != nil {
		t.Fatalf("building the ipc guest: %v", err)
	}
	dir := packageIPCGuest(t, root, tmp, out)

	got, err := runIPCHost(t, h, dir)
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, got)
	}
	want := []string{
		// _initialize ran and the subscription was made, and NOTHING went out.
		"CHECK initialize-sent 0",
		// The first pump says HELLO, once, in the literal-concat form.
		"CHECK tick1-sent 1",
		"CHECK localised-shape ok",
		"CHECK frame-type 1",
		// ...for the server, which is the arm the probe verified working.
		"CHECK send-for-player 0",
		"CHECK recv-for-player 0",
		// The token comes back and the guest adopts it.
		"LOG fkipc session up",
		// A request in, an echo out, all 256 byte values byte-exact through the
		// event encode, the guest's decode, and back out through send_udp.
		"CHECK echo-bytes ok",
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	for _, w := range want {
		found := false
		for _, l := range lines {
			if l == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing %q from:\n%s", w, got)
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "CHECK") && strings.HasSuffix(l, "BAD") {
			t.Errorf("failed check: %s", l)
		}
	}
}

// BELOW THE ENGINE FLOOR THE SAME MOD IS INERT, through the same runtime, with
// one string different in the stub.
//
// This is the arm the hard-disable exists for and it is the only place it can
// be checked end to end: the library reads helpers.game_version through the
// generated bindings, so nothing host-side of the wasm boundary exercises the
// path a mod actually takes. Three claims, and the first is the whole feature:
//
//   - send_udp is NEVER CALLED. Not "only HELLOs" -- none. The send-only design
//     this replaced would have put one on the wire on the first pump and
//     another every SearchTicks forever, so `#sent == 0` after sixty pumps is
//     the assertion that distinguishes the two designs and nothing weaker is.
//   - recv_udp is never called either, which is the original safety property:
//     on 2.0.77 a headless recv_udp with a packet queued aborts the process in
//     C++, and a queued packet is exactly what this arm leaves sitting there.
//   - the one log line is present, so an author on an old engine is told why
//     their mod does nothing instead of having to read a counter.
//
// It runs the SAME PACKAGED MOD as the arm above rather than a second build:
// what differs between a working mod and an inert one must be the engine and
// nothing else, and building twice would leave room for it not to be.
func TestBelowTheEngineFloorTheModIsInertThroughTheRealRuntime(t *testing.T) {
	h := needGuest(t)
	root, tmp := repoRoot(t), t.TempDir()
	out := filepath.Join(tmp, "ipc.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/ipc", out); err != nil {
		t.Fatalf("building the ipc guest: %v", err)
	}
	dir := packageIPCGuest(t, root, tmp, out)

	got, err := runIPCHostBelowFloor(t, h, dir)
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, got)
	}
	for _, w := range []string{
		"CHECK sent 0",
		"CHECK recv-called no",
		"CHECK inbox-untouched 1",
		"LOG fkipc: disabled -- requires Factorio >= 2.1.14; this engine is 2.0.77",
		"CHECK log-lines 1",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q from:\n%s", w, got)
		}
	}
}

func packageIPCGuest(t *testing.T, root, tmp, wasmPath string) string {
	return packageIPCGuestWith(t, root, tmp, wasmPath, luagen.Options{})
}

// packageIPCGuestWith is packageIPCGuest with the emitter options spelled out,
// because not every fkipc test wants the same chunk. The wire tests take the
// zero value -- no persistence, level 0 -- since what they assert is bytes on a
// socket and a smaller chunk builds faster. The JOIN test cannot: what it
// compares IS storage.fk_mem, which only exists under --persist=table, and
// state_load only adopts a heap whose BuildID matches, so both have to be said.
func packageIPCGuestWith(t *testing.T, root, tmp, wasmPath string, opts luagen.Options) string {
	t.Helper()
	raw, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := wasm.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(mod)
	if err != nil {
		t.Fatal(err)
	}
	src, err := luagen.EmitModuleWith(im, opts)
	if err != nil {
		t.Fatal(err)
	}
	a, err := factorio.LoadAPI(filepath.Join(root, "api",
		factorio.DefaultAPIVersion, "runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, events := factorio.GenerateMembers(a), factorio.GenerateEvents(a)
	used, _ := factorio.UsedMembers(im)
	usedEv, _ := factorio.UsedEvents(im)
	table, err := report.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fk-ipc", Version: "0.1.0", Title: "FkLua IPC",
			Author: "FkLua", FactorioVersion: factorio.DefaultFactorioVersion,
		},
		Chunk: src, APITable: table,
	}
	for _, e := range im.Exports {
		pkg.Exports = append(pkg.Exports, e.Name)
	}
	dir, err := pkg.WriteDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// The Lua host. Everything the mod touches, plus a `helpers` that behaves the
// way Factorio's does: send_udp records the LocalisedString it was handed, and
// recv_udp DELIVERS QUEUED DATAGRAMS AS EVENTS, on the caller's tick, through
// the registered on_udp_packet_received dispatcher -- which is the shape the
// engine has and the shape a convenient stub would not.
//
// The version it reports is MinEngineVersion, because below the floor the
// library is inert and this test would be asserting nothing. The sub-floor arm
// is TestBelowTheEngineFloorTheModIsInertThroughTheRealRuntime, which runs the
// SAME packaged mod against a stub differing in that one string.
func runIPCHost(t *testing.T, h *luahost.Host, dir string) (string, error) {
	t.Helper()
	return h.RunString(fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
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

-- Little-endian by hand, both ways. The frame header is what the guest and an
-- external tool agree on, so a test that built it with a helper the guest also
-- used would be checking itself.
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

-- THE ENGINE-SHAPED helpers. No self anywhere: __index hands back a closure
-- that already carries the object, so a member's declared arguments are all it
-- gets -- and the arity is exact, which is what these asserts model.
local sent, inbox, tick = {}, {}, 0
local recvFP = "unset"
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

-- Open ran inside _initialize and must have sent NOTHING: control.lua's main
-- chunk is where a non-zero for_player is silently skipped, and the library's
-- rule is that the first frame leaves from the first Pump.
print("CHECK initialize-sent " .. #sent)

local function pump()
  tick = tick + 1
  handlers[EV_TICK]({ tick = tick })
end

pump()
print("CHECK tick1-sent " .. #sent)
local first = sent[1]
if type(first.data) == "table" and #first.data == 2 and first.data[1] == ""
   and type(first.data[2]) == "string" then
  print("CHECK localised-shape ok")
else
  print("CHECK localised-shape BAD " .. type(first.data))
end
print("CHECK send-for-player " .. tostring(first.fp))
print("CHECK recv-for-player " .. recvFP)

local hello = first.data[2]
if hello:sub(1,2) ~= "FK" then print("CHECK magic BAD") end
print("CHECK frame-type " .. hello:byte(4))
local helloCorr = u32(hello, 17)

-- HELLO_ACK, matched on the HELLO's corr and carrying an epoch the guest has
-- never seen -- which is the one frame whose epoch test is skipped, by
-- definition, because it is what mints the epoch.
local EPOCH = 0x51C0FFEE
-- The name is the guest's IDENTITY TOKEN, and examples/ipc requires it: the
-- fixture sets Config.ExpectPeer = "fk-ipc/1", so an ACK carrying anything else
-- is refused rather than adopted. The refusing direction is covered by
-- TestBothGuestLibrariesSpeakTheSameWire and the two libraries' own suites; here
-- the matched token keeps this test about the RUNTIME.
local ackBody = string.char(1, 1) .. p16(2048) .. p16(16) .. p32(0) .. p32(tick) ..
                string.char(0, 0) .. p16(8) .. "fk-ipc/1"
inbox[#inbox+1] = frame(2, 0, 0, EPOCH, 0, helloCorr, 0, 1, ackBody)
pump()

-- All 256 byte values, as a REQ on the control channel. NUL does not truncate
-- and a high byte is not UTF-8-mangled -- measured on the real transport, and
-- this is the same claim about the four layers of marshalling between it and
-- the guest's handler.
local all = {}
for i = 0, 255 do all[#all+1] = string.char(i) end
all = table.concat(all)
inbox[#inbox+1] = frame(5, 0, 2, EPOCH, 1, 77, 0, 1, all)
pump()
pump()

local echoed = nil
for i = 1, #sent do
  local f = sent[i].data[2]
  if f:byte(4) == 6 and u16(f, 7) == 2 then   -- RESP on channel 2
    echoed = f:sub(25, 25 + u16(f, 21) - 1)
  end
end
if echoed == all then
  print("CHECK echo-bytes ok")
else
  print("CHECK echo-bytes BAD " .. tostring(echoed and #echoed))
end
`, filepath.Join(dir, "?.lua")))
}

// runIPCHostBelowFloor is runIPCHost's stub with the version string moved below
// the floor and the conversation removed, because below the floor there is no
// conversation to have.
//
// A DATAGRAM IS LEFT QUEUED THROUGHOUT, which is the point rather than
// thoroughness: on 2.0.77 it is a recv_udp WITH A PACKET WAITING that aborts
// the process in C++, and a stub with an empty inbox would pass whether or not
// the gate worked. Here recv_udp raises outright if it is ever called, so the
// safety property is a hard failure and not a counter this test could forget to
// read.
func runIPCHostBelowFloor(t *testing.T, h *luahost.Host, dir string) (string, error) {
	t.Helper()
	return h.RunString(fmt.Sprintf(`package.path = %q
local logs = 0
function log(s) logs = logs + 1 print("LOG " .. s) end
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

local sent, inbox, tick, recvCalled = {}, {}, 0, false
local function arity(n, f)
  return function(...)
    if select("#", ...) ~= n then
      error("Arguments count error: got " .. select("#", ...) .. " want " .. n, 0)
    end
    return f(...)
  end
end
helpers = {
  -- THE ONE STRING THAT DIFFERS from the working arm.
  game_version = "2.0.77",
  send_udp = arity(3, function(port, data, for_player)
    sent[#sent+1] = { port = port, data = data, fp = for_player }
  end),
  recv_udp = arity(1, function(for_player)
    -- Not a counter: on this engine this call with a packet queued is a C++
    -- abort no pcall can catch, so the stub models it as the unrecoverable
    -- thing it is.
    recvCalled = true
    error("recv_udp was called below the engine floor", 0)
  end),
  write_file = arity(4, function(name, data, append, for_player) end),
}

-- A packet waiting from the very start, because the crash needs BOTH the pump
-- call and something queued.
inbox[#inbox+1] = "FK\1\1"

require("control")
if handlers.on_init then handlers.on_init() end

-- Sixty pumps is a full SearchTicks window and then some: the send-only design
-- put a HELLO on the first tick and another every sixtieth, so anything above
-- zero here is that design still present.
for i = 1, 60 do
  tick = tick + 1
  handlers[EV_TICK]({ tick = tick })
end

print("CHECK sent " .. #sent)
print("CHECK recv-called " .. (recvCalled and "YES-BAD" or "no"))
print("CHECK inbox-untouched " .. #inbox)
print("CHECK log-lines " .. logs)
`, filepath.Join(dir, "?.lua")))
}
