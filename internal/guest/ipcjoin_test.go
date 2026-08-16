package guest_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// joinOpts is what `fklua mod` defaults to, and every part of it is load-bearing
// here. --persist=table is the mode in which guest memory IS storage.fk_mem,
// i.e. the mode in which a divergence is a CRC failure rather than a private
// discrepancy; -opt=3 is the level a real mod ships at, so the comparison runs
// over the inlined stores and the loop guards rather than over level 0's helper
// calls; and a BuildID has to exist at all, because state_load refuses to adopt
// a heap whose build stamp does not match and a joiner that adopted nothing
// would pass this test by starting from the same fresh memory the server would
// never have.
var joinOpts = luagen.Options{
	Persist: luagen.PersistTable,
	Opt:     analysis.O3,
	BuildID: "join-parity",
}

// A joinArm is one guest driven through the harness below.
//
// FOUR ARMS, AND THE SECOND PAIR IS THE POINT OF HAVING A TABLE AT ALL.
// examples/{go,rust}/ipc are the WIRING fixtures: they exist to prove the four
// exports and the library behind them, and their handlers deliberately touch
// nothing but a byte buffer. The demo mods are AUTHOR-SHAPED: their handlers
// mutate the world through the API, read it back, assemble telemetry out of
// what they read, and answer RPCs that change what the next read returns. That
// is the code a mod is made of, and until it was in here nothing said a guest
// written that way stays byte-identical across a join.
type joinArm struct {
	name string
	// lang picks the toolchain, and pkg is what that toolchain calls the guest:
	// a Go package path, or a cargo package name.
	lang, pkg string

	// port is the guest's Config.Port -- the COMPANION's port, which the link
	// tests every datagram's source against. It differs per arm and getting it
	// wrong is silent: every frame reads as another mod's and the session never
	// comes up, which the baseline check below would not catch.
	port uint16
	// peer is what the harness's HELLO_ACK calls itself, and it must equal the
	// guest's Config.ExpectPeer. The demo mods set one; so does examples/ipc.
	peer string

	// reqs are the three REQ payloads the harness sends -- one before the save
	// and two inside the joined window. THEY ARE PER ARM BECAUSE THE HANDLER IS:
	// a payload the demo mods cannot parse is answered with `err` and changes
	// nothing, which would leave the arm exercising the library and none of the
	// guest above it.
	reqs [3]string

	// wantApp is what the LAST telemetry frame of the joined window must
	// contain, and it is the anti-vacuity assertion. Every failure mode this
	// harness has -- a stub method whose arity is wrong, a world the guest
	// cannot reach, a request it refused -- leaves BOTH peers doing nothing,
	// identically, and a test asserting only "identical" would report a green it
	// had not earned. So the arm states what the guest must have computed out of
	// the world it was given.
	wantApp []string
}

var joinArms = []joinArm{
	{
		name: "ipc-go", lang: "go", pkg: "./examples/ipc",
		port: 25411, peer: "fk-ipc/1",
		reqs:    [3]string{"echo me", "echo two", "echo three"},
		wantApp: []string{"tick=120"},
	},
	{
		name: "ipc-rust", lang: "rust", pkg: "ipc",
		port: 25411, peer: "fk-ipc/1",
		reqs:    [3]string{"echo me", "echo two", "echo three"},
		wantApp: []string{"tick=120"},
	},
	{
		// The slider mod. Its handler stores what was asked, fk_on_tick applies
		// it to the surface, and the telemetry reports the READBACK -- so every
		// substring below is a round trip through the host and back into guest
		// memory. px/py come from the stub player's position, which is the one
		// value in the frame the guest cannot have made up.
		name: "demo-daylight", lang: "go", pkg: "./examples/demo-daylight",
		port: 29434, peer: "fk-demo-daylight/1",
		reqs: [3]string{"set daytime 640", "set daytime 200", "set daytime 900"},
		wantApp: []string{"tick=120", "daytime=900", "frozen=1", "player=1",
			"px=-350", "py=1225"},
	},
	{
		// The circle mod, and it is the widest surface of the four: a render
		// object fetched by an id that lives in GUEST memory across the join, a
		// dictionary attribute walked for the enemy force, and an entity count
		// whose answer moves with the radius the other slider just set. entities
		// is floor(radius * 2) by the stub's own rule, so 40 -> 80 says the
		// applied radius reached the host and came back.
		name: "demo-circle", lang: "rust", pkg: "demo-circle",
		port: 29437, peer: "fk-demo-circle/1",
		reqs: [3]string{"set radius 24", "set hue 210", "set radius 40"},
		wantApp: []string{"tick=120", "radius=40", "hue=210", "evo=134000",
			"entities=80"},
	},
}

// A JOINING MULTIPLAYER CLIENT MUST STAY BYTE-IDENTICAL TO THE SERVER, AND
// THAT IS WHAT MAKES `fk_after_load` A TRAP RATHER THAN A HOOK.
//
// Factorio runs script.on_load on EVERY PEER THAT LOADS THE STATE -- which
// includes a client joining a game already in progress. `fk_mod.lua` arms its
// `fk_after_load` one-shot from there, so the joiner dispatches that export on
// its first simulated tick and the server, which did so long ago, does not.
// Anything the export writes to guest memory is therefore written on ONE PEER
// ONLY -- and under the default --persist=table guest memory IS
// `storage.fk_mem`, which Factorio CRCs. One write, one desync, and the game
// says `Multiplayer desynchronisation: crc test failed` from the very next
// tick.
//
// `fk_mod.lua` already says the rule for the hook one level up -- "on_load is
// READ-ONLY with respect to storage, and has to be: Factorio runs it on every
// client when joining a multiplayer game, and a write here is a desync waiting
// to happen" -- and the one-shot inherits it, because a one-shot armed from
// on_load is a write from on_load with one tick of delay.
//
// So this builds the SERVER and then the JOINER the way the engine does:
//
//	A: a fresh module instance, on_init, a full handshake and traffic;
//	   `storage` is deep-copied at that point, which is the save a joining
//	   client downloads;
//	B: a fresh module instance over that copy -- _initialize rebuilds and
//	   state_load replaces it -- and then on_load, WHICH ARMS THE ONE-SHOT;
//	both are then driven with the SAME ticks and the SAME inbound datagrams,
//	because an inbound datagram is an InputAction and lands at every peer at
//	the same tick. That identity is what makes the epoch handshake legal at
//	all (agents/ipc.md, the cost model), so it is the right thing to hold
//	fixed here.
//
// The assertion is on GUEST MEMORY and deliberately not on what either peer
// sent: outbound is a local side effect that never enters game state, and a
// client's send_udp is a measured silent no-op. Memory is the CRC.
//
// It fails against the load-reset design at the first joined tick, with
// fk_mem's `boot` word as the first divergence.
//
// THE JOINED WINDOW CARRIES INBOUND TRAFFIC, and until P12 was fixed it could
// not. That defect was the HOST's rather than fkipc's -- fk_mod.lua cached its
// per-nesting-level event scratch buffer in a Lua local, which every load
// rebuilds empty while the guest heap comes back from the save, so the joiner
// allocated a second buffer beside the server's and every allocation it made
// afterwards landed event_scratch bytes further up. It had its own arm here, a
// phase 2 written to fail the day somebody fixed it; the buffer caches are
// mirrored through `storage.fk_bufs` now, so the arm is gone and its traffic is
// where it belongs. What that buys is that the window covers EVENT-CARRYING
// dispatches -- an inbound datagram is delivered from inside the guest's own
// pump, so it is a NESTED dispatch and it is the level-2 buffer that has to
// line up, which is the case the host was getting wrong.
//
// One frame in the window comes from a FOREIGN SOURCE PORT and it is the
// attribution leg, kept from that phase 2 verbatim in intent: the guest library
// drops such a frame before it reads a single header byte, so nothing fkipc
// owns can be what moves, while the host encodes the event regardless. If the
// buffer mirror ever regresses, that tick is the one that says so about the
// host and about nothing else.
//
// AND THE CORPUS IS FOUR GUESTS RATHER THAN TWO, since the demo mods joined it.
// The wiring fixtures could only ever have caught a defect in the library or in
// the runtime under it, because their handlers touch nothing else; the demo
// arms drive a handler that calls the API, stores what it read, and streams a
// frame assembled out of the world. That is where an author's own peer-local
// write would live, and it is the shape scripts/run-ipcdemo.sh --play desynced
// on for real before the send-status fix.
//
// AND IT RUNS IN TWO MODES OFF ONE BUILD, WHICH IS THE STALE ARM.
//
// In `fresh` the server is a new map, which is every run this harness made until
// 2026-08-07. In `stale` the server LOADS A SAVE WRITTEN BY ANOTHER BUILD and
// gets no on_configuration_changed -- which is not an exotic case, it is what
// every dev rebuild is, and what the commit that folded the --api pin into the
// build stamp made of every cached map in existence at once.
//
// What the unfixed runtime does with it is the sharpest thing in this file.
// state_load declines and correctly does not write `storage` from on_load; the
// only code that ever republished the stamp lived in the hook that did not
// fire; so the SERVER runs on a fresh heap for twenty minutes while `storage`
// still holds the previous build's, and a joining client downloads that same
// stale stamp, declines identically, and starts a TICK-0 heap against a server
// at tick 1250. Measured live on 2026-08-07: crc test failed from the first
// joined tick, repeating, with no warning on either peer.
//
// THE TICK COMPARISON CANNOT SEE IT, and saying so is the point of the three
// extra assertions the stale arm carries. When neither peer republishes, both
// peers' `storage` holds a deep copy of the same frozen heap that neither of
// them is running on: every tick compares identical, and the harness would
// report a green over two guests with nothing in common. So the stale arm
// asserts on the SERVER -- that the stamp moved, that the checksum of
// storage.fk_mem moved across the window, and that the author was told -- and
// only then reads the joined window as meaning anything.
//
// THE JOINER'S OUTBOUND SURFACE FAILS, WHICH IS THE OTHER HALF AND IS NEW.
// --enable-lua-udp binds the game's socket and a graphical client joining a
// headless server is not started with it, so send_udp answers differently on
// the two peers on every frame of the session -- the exact condition that
// produced the measured desync, with no companion and no inbound datagram
// anywhere near it. The stub records what the guest assembled and THEN refuses,
// so the harness still sees the buffer while the guest sees only the error.
// Confirmed by mutation: restoring `if l.tr.Send(f) == StatusOK { TxFrames++ }
// else { QueueDrops++ }` in guest/go/fkipc turns the demo-daylight arm red at
// tick 90 on three words -- TxFrames 6 vs 5, TxBytes 330 vs 292, QueueDrops 0
// vs 1 -- where before this arm existed the same mutation was invisible here.
// The library's structural answer is that the transport seam returns NOTHING;
// this is where that is measured rather than declared.
func TestAJoiningPeerStaysByteIdenticalToTheServer(t *testing.T) {
	h := needGuest(t)
	root := repoRoot(t)

	for _, arm := range joinArms {
		t.Run(arm.name, func(t *testing.T) {
			tmp := t.TempDir()
			var w string
			switch arm.lang {
			case "go":
				w = filepath.Join(tmp, "guest.wasm")
				if err := guest.Build(filepath.Join(root, "guest", "go"), arm.pkg, w); err != nil {
					t.Fatalf("building the Go guest %s: %v", arm.pkg, err)
				}
			case "rust":
				if ok, why := guest.RustAvailable(); !ok {
					t.Skipf("skipping the Rust arm: %s", why)
				}
				var err error
				w, err = guest.BuildRust(filepath.Join(root, "guest", "rust"), arm.pkg,
					filepath.Join(tmp, "cargo"))
				if err != nil {
					t.Fatalf("building the Rust guest %s: %v", arm.pkg, err)
				}
			default:
				t.Fatalf("unknown lang %q", arm.lang)
			}
			dir := packageIPCGuestWith(t, root, tmp, w, joinOpts)
			// TWO MODES OFF ONE BUILD. The toolchain build is the whole cost of
			// this test; the Lua run is milliseconds, so the stale arm is
			// essentially free and there is no reason for it to be a second
			// test that pays for a second build.
			t.Run("fresh", func(t *testing.T) { checkJoinParity(t, h, dir, arm, false) })
			t.Run("stale", func(t *testing.T) { checkJoinParity(t, h, dir, arm, true) })
		})
	}
}

// bootFresh is the server as this harness has always built it: a NEW MAP, so
// on_init runs, state_init publishes, and there is no rebuild anywhere near it.
const bootFresh = `local A = fresh("A", {}, newworld(nil), true)
if A.h.on_init then A.h.on_init() end
print("INFO server booted fresh")`

// ...and bootStale is the server loading a save WRITTEN BY ANOTHER BUILD, with
// no on_configuration_changed, which is what a dev rebuild looks like to the
// engine. See the stale arm's header on TestAJoiningPeerStaysByteIdenticalToThe-
// Server for why that combination is the one that desynced a real game.
//
// The stamp is tampered rather than the guest being built twice, which is the
// same comparison read from the other side: same_build() is
// `storage.fk_build == P.build`, and a rebuild is exactly the case where those
// two strings differ. bufpin_test.go's C leg is the precedent.
//
// Z's WORLD is handed to A because a reload keeps the world -- only the mod
// changed.
const bootStale = `local Z = fresh("Z", {}, newworld(nil), true)
if Z.h.on_init then Z.h.on_init() end
for t = 1, 5 do tickpeer(Z, t, {}) end
local staleSave = deepcopy(Z.storage)
staleSave.fk_build = "another-build-of-this-guest"
local A = fresh("A", staleSave, newworld(Z.world), true)
if A.h.on_load then A.h.on_load() end
-- AND NO on_configuration_changed. Factorio raises it when the mod SET changes,
-- which for one mod means its VERSION moving; a rebuild keeps the version.
print("INFO server booted stale")`

func checkJoinParity(t *testing.T, h *luahost.Host, dir string, arm joinArm, stale bool) {
	t.Helper()
	boot := bootFresh
	if stale {
		boot = bootStale
	}
	script := strings.NewReplacer(
		"@@PATH@@", filepath.Join(dir, "?.lua"),
		"@@PORT@@", strconv.Itoa(int(arm.port)),
		"@@PEER@@", arm.peer,
		"@@REQ1@@", arm.reqs[0],
		"@@REQ2@@", arm.reqs[1],
		"@@REQ3@@", arm.reqs[2],
		"@@BOOTA@@", boot,
	).Replace(joinParityScript)

	out, err := h.RunString(script)
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}

	joins, bad := 0, 0
	sawBaseline, inbound := false, ""
	stamp, memsum := "", ""
	app := map[string]string{}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "INFO stamp "):
			stamp = strings.TrimPrefix(l, "INFO stamp ")
			t.Log(l)
		case strings.HasPrefix(l, "INFO memsum "):
			memsum = strings.TrimPrefix(l, "INFO memsum ")
			t.Log(l)
		case strings.HasPrefix(l, "INFO inbound "):
			inbound = strings.TrimPrefix(l, "INFO inbound ")
			t.Log(l)
		case strings.HasPrefix(l, "INFO app A "):
			app["A"] = strings.TrimPrefix(l, "INFO app A ")
			t.Log(l)
		case strings.HasPrefix(l, "INFO app B "):
			app["B"] = strings.TrimPrefix(l, "INFO app B ")
			t.Log(l)
		case strings.HasPrefix(l, "INFO "):
			t.Log(l)
		case l == "JOIN baseline identical":
			sawBaseline = true
		case strings.HasPrefix(l, "JOIN baseline "):
			t.Fatalf("the harness is wrong before the test even starts -- the "+
				"joiner did not adopt the server's memory: %s\n%s", l, out)
		case strings.HasPrefix(l, "JOIN tick "):
			joins++
			if !strings.HasSuffix(l, " identical") {
				bad++
				if bad <= 3 {
					t.Errorf("A JOINING PEER DIVERGED FROM THE SERVER: %s", l)
				}
			}
		}
	}
	if !sawBaseline {
		t.Fatalf("the script never reached the join:\n%s", out)
	}
	if stale {
		// THE SERVER MUST HAVE FINISHED THE REBUILD BEFORE THE JOINER ARRIVES,
		// and these two are the only things that say so -- the tick-by-tick
		// comparison below CANNOT. When neither peer republishes, both hold a
		// deep copy of the same frozen build-Z heap in `storage` and their own
		// live memory somewhere `storage` cannot see: every tick compares
		// identical, and the harness reports a green over two guests that have
		// nothing to do with each other. That is the vacuity this whole file is
		// written against, met from a new direction.
		if stamp == "" || stamp == "another-build-of-this-guest" {
			t.Errorf("THE SERVER LEFT THE SAVE CARRYING THE STAMP IT DECLINED "+
				"(fk_build=%q). Nothing republished it, because the only thing "+
				"that ever did was on_configuration_changed and Factorio does not "+
				"raise that for a rebuild which keeps the mod's version. So the "+
				"state this joiner downloaded still says another build wrote it, "+
				"the joiner declines exactly as the server did, and it starts a "+
				"TICK-0 heap against a server 70 ticks in -- which is a desync "+
				"from the first joined tick and was measured as one.", stamp)
		}
		half := strings.Fields(memsum)
		if len(half) != 2 || half[0] == half[1] {
			t.Errorf("THE SERVER'S SAVE IS NOT TRACKING ITS GUEST: fk_mem "+
				"checksums %q before and after 65 ticks of traffic. state_load "+
				"declined and nothing republished the mirror, so `storage` holds "+
				"the previous build's heap while the guest runs on the fresh one "+
				"_initialize built -- two unrelated tables, and every write the "+
				"guest makes reaches neither the save nor the multiplayer CRC.",
				memsum)
		}
		// ...and the author is told, which is the third thing the hook that did
		// not fire was carrying. None of these guests exports a migrate hook, so
		// the channel is the log line.
		if !strings.Contains(out, "LOG A fklua: this mod was rebuilt") {
			t.Errorf("the server ran a rebuilt guest and said nothing about it. " +
				"The notice -- and the fk_migrate dispatch beside it -- lived in " +
				"on_configuration_changed, so a same-version rebuild reset every " +
				"user's guest state silently.")
		}
	}
	if joins < 12 {
		t.Fatalf("only %d joined ticks were compared; the script cannot have "+
			"run:\n%s", joins, out)
	}
	// THE WINDOW HAS TO HAVE CARRIED INBOUND, checked rather than assumed. The
	// event-carrying dispatch is the case the host was getting wrong, and a
	// harness change that quietly stopped delivering datagrams would leave every
	// tick trivially identical and this test reporting a green it had not
	// earned. The wanted count is counted BY THE SCRIPT off the schedule it
	// drives from, so the number lives in one place; what is asserted here is
	// that both peers reached it and that it is not zero.
	got := strings.Fields(inbound)
	if len(got) != 3 || got[0] != got[2] || got[1] != got[2] || got[2] == "0" {
		t.Fatalf("the joined window delivered %q, where the three counts are "+
			"server, joiner and the schedule's own total -- an inbound frame is "+
			"what exercises the nested dispatch and the per-level event "+
			"buffer:\n%s", inbound, out)
	}
	// ...AND THE GUEST HAS TO HAVE DONE ITS OWN WORK. Everything this harness
	// can get wrong -- a stub method with the wrong arity, a request the handler
	// refused, a world the guest never reached -- fails on BOTH peers in the same
	// way, so "identical" survives it and means nothing. The last telemetry frame
	// of the window is the guest's own statement about what it computed out of
	// the world, and it is compared across the two peers as well, because it is
	// assembled in a buffer that IS guest memory.
	if app["A"] == "" || app["B"] == "" {
		t.Fatalf("no telemetry frame reached the wire in the joined window "+
			"(server %q, joiner %q) -- the guest above the library did nothing, "+
			"so the tick comparison above is vacuous:\n%s", app["A"], app["B"], out)
	}
	if app["A"] != app["B"] {
		t.Errorf("the two peers assembled DIFFERENT telemetry out of the same "+
			"ticks and the same datagrams:\n  server: %s\n  joiner: %s",
			app["A"], app["B"])
	}
	for _, w := range arm.wantApp {
		if !strings.Contains(app["A"], w) {
			t.Errorf("the last telemetry frame is %q and does not contain %q -- "+
				"the guest did not complete the round trip this arm exists to "+
				"drive", app["A"], w)
		}
	}
}

// The Lua host. TWO INDEPENDENT MODULE INSTANCES IN ONE INTERPRETER, which is
// what a server and a joining client are: same bytes, separate state.
//
// `cur` is the peer whose code is running, and every global the runtime reaches
// through -- `storage`, the event dispatcher table, the outbound record, and
// the WORLD -- goes through it. Clearing all four `package.loaded` entries
// before each require is what makes the second instance genuinely fresh rather
// than a second reference to the first: `fk_module` returns a factory so its own
// instance would be new either way, but `fk_abi` is a cached module table and
// two peers sharing one handle space would be a fidelity hole exactly where this
// test is looking.
//
// `helpers`, `game` and `rendering` are Factorio's shape, not a convenience:
// methods take their declared arguments with no self, because __index hands back
// a closure that already carries the object, and each asserts its arity -- as a
// range over the OPTIONAL tail, which is a measurement rather than a softening.
// See the arity helper inside the script.
const joinParityScript = `package.path = "@@PATH@@"

local EV_TICK, EV_UDP = 1, 2
defines = { events = { on_tick = EV_TICK, on_udp_packet_received = EV_UDP } }

local cur
function log(s) print("LOG " .. cur.name .. " " .. s) end

script = {
  mod_name = "fk-ipc",
  on_init = function(f) cur.h.on_init = f end,
  on_load = function(f) cur.h.on_load = f end,
  on_configuration_changed = function(f) cur.h.on_config = f end,
  on_event = function(ev, f, flt) cur.h[ev] = f end,
}

-- THE ARITY CHECK, AND IT IS A RANGE OVER THE OPTIONAL TAIL.
--
-- A Factorio method is a BOUND closure handed back by __index: it takes its
-- declared arguments and no self, and its count is exact (CLAUDE.md, "A
-- Factorio method is a BOUND closure"). What is NOT exact is a trailing
-- OPTIONAL argument the guest omits -- it is genuinely absent rather than nil,
-- measured here rather than assumed: ProfileServer calls send_udp with three
-- arguments and recv_udp with one, and ProfileClient calls them with TWO and
-- ZERO. That difference is the whole point of the two profiles on this surface,
-- and a stub asserting the server's count makes every client-profile call raise
-- inside fk_abi's pcall, come back as ERR_CALL_FAILED, and leave the guest
-- silently doing nothing -- which is how the demo arms first "passed".
--
-- So lo is the mandatory count and hi is mandatory + optional. An extra
-- argument, a missing mandatory one, or a self smuggled in front still fails.
local function arity(lo, hi, f)
  return function(...)
    local n = select("#", ...)
    if n < lo or n > hi then
      error("Arguments count error: got " .. n .. " want " .. lo .. ".." .. hi, 0)
    end
    return f(...)
  end
end

helpers = {
  game_version = "2.1.14",
  -- THE SERVER HAS A SOCKET AND THE JOINER DOES NOT, and that asymmetry is not
  -- decoration: --enable-lua-udp is what binds the game's UDP socket, a
  -- headless server in this project is started with it and a graphical client
  -- joining that server is not. So the OUTBOUND surface answers differently on
  -- the two peers -- every tick, for the life of the session -- and under
  -- --persist=table anything the guest stores about that answer is a word in
  -- storage.fk_mem that Factorio CRCs.
  --
  -- That is the second half of the peer-local rule and it desynced a real game:
  -- an "if the send returned OK then TxFrames++ else QueueDrops++" shipped,
  -- and a client joining a server running the demo mods desynced on the first
  -- tick it simulated with NO companion anywhere and no inbound datagram in the
  -- game. The library answers it by having no value to branch on -- the
  -- transport seam's Send and WriteFile return nothing at all, pinned by
  -- guest/{go,rust}/fkipc's own text-property test -- and this is where that
  -- claim is measured rather than declared.
  --
  -- RECORDED FIRST, THEN REFUSED. The record is the harness's view of what the
  -- guest ASSEMBLED, which is a fact about guest memory and must match; the
  -- refusal is what the engine tells the guest, which must reach nothing.
  send_udp = arity(2, 3, function(port, data, for_player)
    local f = data[2]
    cur.sent[#cur.sent + 1] = f
    -- THE APPLICATION'S OWN LAST WORD. A MSG on channel 1 is what all four
    -- guests stream their telemetry on, and its payload is assembled in a buffer
    -- that IS guest memory -- so recording it here costs nothing and gives the
    -- Go side something to assert that is not "the two peers both did nothing".
    -- Header: 2 magic, 1 version, 1 type, 2 flags, 2 channel, ... 24 in all.
    if #f > 24 and f:byte(4) == 4 and f:byte(7) + f:byte(8) * 256 == 1 then
      cur.app = f:sub(25)
    end
    if not cur.udp then
      error("send_udp: this peer has no socket bound (--enable-lua-udp)", 0)
    end
  end),
  recv_udp = arity(0, 1, function(for_player)
    -- One call drains the whole backlog as a batch of events, which is what the
    -- probe measured. Every peer is handed the SAME datagrams at the SAME tick,
    -- because an inbound datagram is an InputAction and the engine replicates
    -- it -- that identity is the premise the whole protocol is built on.
    --
    -- DELIVERED FROM INSIDE THE CALL, which is not a detail: the pump is
    -- already an outermost dispatch, so an event raised here is a NESTED one
    -- and the host's per-level event buffer is level 2 rather than level 1. A
    -- stub that raised the event from outside the pump would exercise a
    -- different buffer than the game does.
    while #cur.inbox > 0 do
      local d = table.remove(cur.inbox, 1)
      cur.recvd = cur.recvd + 1
      cur.h[EV_UDP]({ payload = d.p, source_port = d.src, player_index = 0,
                      name = EV_UDP, tick = cur.tick })
    end
  end),
  -- Refused on the joiner alongside the send, because both are the same fact
  -- about the same peer -- 2.1 documents a non-zero for_player as silently
  -- skipped from some stages, and a client is not the server. WriteBulk's own
  -- shape is what this protects: it used to return early on a failed write and
  -- skip the FILE_NOTIFY, which consumes the channel's seq, so one peer would
  -- advance the counter and the other would not.
  write_file = arity(2, 4, function(name, data, append, for_player)
    if not cur.udp then
      error("write_file: this peer cannot write here", 0)
    end
  end),
}

-- ---------------------------------------------------------------------------
-- THE WORLD, AND THERE IS ONE PER PEER.
--
-- The demo arms are what this exists for. Their handlers do not merely decode a
-- payload: they set a surface's daytime, resize a render object, count entities
-- inside it and read a force's evolution back out, then assemble a telemetry
-- frame from what they read. A stub that could not carry that would make those
-- arms pass by never running, which is the failure mode this whole file is
-- built to refuse.
--
-- EACH PEER GETS ITS OWN WORLD, which costs one constructor and is the whole
-- fidelity of the arm: a shared one lets a write by the server repair a read by
-- the joiner, so the comparison would silently lose the ability to see the
-- divergence it is looking for. In a real game the two worlds are separate
-- objects that stay identical BY DETERMINISM, and B's is downloaded with the
-- save -- which is why the constructor takes the server's world and copies its
-- mutable state rather than starting from the defaults.
--
-- METHODS ARE ARITY-CHECKED, and that is not decoration. A Factorio method is a
-- BOUND closure handed back by __index and its argument count is exact
-- (CLAUDE.md, "A Factorio method is a BOUND closure"); getting it wrong here
-- would raise inside fk_abi's pcall, come back as ERR_CALL_FAILED, and leave the
-- guest quietly doing nothing on BOTH peers -- identical, green, and worthless.
-- With the check, that shows up as a telemetry frame the Go side rejects.
local function newworld(from)
  local w = {}

  local fs = from and from.surface
  w.surface = {
    valid = true,
    daytime = fs and fs.daytime or 0.5,
    freeze_daytime = fs and fs.freeze_daytime or false,
  }
  -- Its answer MOVES WITH THE ARGUMENT, so a guest that never applied its
  -- radius reads back a different number than one that did. floor(r * 2) is
  -- arbitrary and is the point: nothing in the guest can compute it without
  -- having reached the host.
  w.surface.count_entities_filtered = arity(1, 1, function(filter)
    return math.floor((filter and filter.radius or 0) * 2)
  end)

  -- A position no guest could invent, so px/py in a telemetry frame is evidence
  -- the read happened.
  w.player = { valid = true, position = { x = -3.5, y = 12.25 } }

  -- game.forces is a dictionary keyed by name, and demo-circle WALKS it looking
  -- for "enemy" rather than indexing, because there is no get_force in the
  -- bindings. Three entries, as a normal map has.
  local function force(evo)
    local f = { valid = true }
    -- Its one parameter is OPTIONAL, and the Rust guest passes nothing, so
    -- the engine sees a bare call.
    f.get_evolution_factor = arity(0, 1, function(surface) return evo end)
    return f
  end
  w.forces = { player = force(0.0), enemy = force(0.134), neutral = force(0.0) }

  -- The render object exists in BOTH worlds from the start, because in a real
  -- game the joiner downloads it with the save: script render objects are game
  -- state. The guest keeps only its ID across the save -- a handle is transient
  -- -- so get_object_by_id is the call the joiner makes and draw_circle is the
  -- one only the server ever made.
  local fr = from and from.render
  w.render = {
    valid = true,
    id = 7,
    radius = fr and fr.radius or 12.0,
    color = fr and fr.color or nil,
  }
  w.rendering = {
    get_object_by_id = arity(1, 1, function(object_id)
      if object_id == w.render.id then return w.render end
      return nil
    end),
    -- takes_table, so it is ONE argument however many fields the description
    -- lists.
    draw_circle = arity(1, 1, function(args)
      w.render.radius = args.radius
      w.render.color = args.color
      return w.render
    end),
  }

  w.game = {
    get_surface = arity(1, 1, function(surface) return w.surface end),
    get_player = arity(1, 1, function(player) return w.player end),
    forces = w.forces,
  }
  return w
end

-- Little-endian by hand, both ways, so a test that built a frame with the
-- guest's own helper would not be checking itself.
local function u32(s, at)
  return s:byte(at) + s:byte(at+1)*256 + s:byte(at+2)*65536 + s:byte(at+3)*16777216
end
local function p16(v) return string.char(v % 256, math.floor(v/256) % 256) end
local function p32(v)
  return string.char(v % 256, math.floor(v/256) % 256,
                     math.floor(v/65536) % 256, math.floor(v/16777216) % 256)
end
local function frame(ty, flags, chan, epoch, seq, corr, frag, nfrag, payload)
  return "FK" .. string.char(1, ty) .. p16(flags) .. p16(chan) .. p32(epoch) ..
         p32(seq) .. p32(corr) .. p16(#payload) .. string.char(frag, nfrag) .. payload
end

local function deepcopy(v)
  if type(v) ~= "table" then return v end
  local o = {}
  for k, val in pairs(v) do o[k] = deepcopy(val) end
  return o
end

-- fk_mem is a VECTOR OF SHARDS, so the walk is shard by shard and indexed
-- rather than through pairs -- a per-element closure over half a million words
-- a tick is the difference between a test and a coffee break.
local function memdiff(ma, mb)
  if type(ma) ~= "table" or type(mb) ~= "table" then return "fk_mem is missing" end
  if #ma ~= #mb then return "shard count " .. #ma .. " vs " .. #mb end
  for s = 1, #ma do
    local a, b = ma[s], mb[s]
    if #a ~= #b then return "shard " .. s .. " length " .. #a .. " vs " .. #b end
    local n, msg = 0, nil
    for i = 1, #a do
      if a[i] ~= b[i] then
        n = n + 1
        if n <= 6 then
          msg = (msg and (msg .. "; ") or "") .. string.format(
            "word %d (shard %d slot %d): %s vs %s",
            (s - 1) * 524288 + i - 1, s, i, tostring(a[i]), tostring(b[i]))
        end
      end
    end
    if msg then return msg .. string.format(" [%d words differ]", n) end
  end
  return nil
end

-- A checksum over the whole shard vector, which is the cheapest honest answer to
-- "did this guest's writes reach the save at all". Position-weighted so that two
-- words swapping places is not the same number, and taken modulo 2^32 rather
-- than left to accumulate into a float whose low bits stop existing.
local function memsum(m)
  if type(m) ~= "table" then return "nil" end
  local s = 0
  for sh = 1, #m do
    local a = m[sh]
    for i = 1, #a do
      s = (s + (a[i] or 0) * ((i % 7) + 1)) % 4294967296
    end
  end
  return string.format("%d", s)
end

local function cmp(a, b, path)
  local ta, tb = type(a), type(b)
  if ta ~= tb then return path .. ": " .. ta .. " vs " .. tb end
  if ta ~= "table" then
    if a ~= b then return path .. ": " .. tostring(a) .. " vs " .. tostring(b) end
    return nil
  end
  for k, v in pairs(a) do
    local r = cmp(v, b[k], path .. "." .. tostring(k))
    if r then return r end
  end
  for k in pairs(b) do
    if a[k] == nil then return path .. "." .. tostring(k) .. ": only on the joiner" end
  end
  return nil
end

-- Everything Factorio would CRC: the guest's linear memory, its mutable globals
-- (the shadow-stack pointer among them), the size mirror, the persistent handle
-- space, and the per-level static buffer caches -- which are in this list
-- because they are in storage, and they are in storage because a Lua local is
-- not load-stable and the heap those addresses point into is. See P12.
local function diffstate(a, b)
  local sa, sb = a.storage, b.storage
  local d = memdiff(sa.fk_mem, sb.fk_mem)
  if d then return "fk_mem " .. d end
  for _, k in ipairs({"fk_globals", "fk_memsize", "fk_build", "fk_state",
                      "fk_handles", "fk_deferred", "fk_gc", "fk_pages",
                      "fk_bufs"}) do
    d = cmp(sa[k], sb[k], k)
    if d then return d end
  end
  return nil
end

-- use makes p the running peer. EVERY global the runtime resolves through moves
-- with it -- storage, and the two world globals, which fk_abi reads live out of
-- _G on every call (M.bind_globals(_G), M.get).
local function use(p)
  cur = p
  storage = p.storage
  game = p.world.game
  rendering = p.world.rendering
end

-- udp says whether THIS peer's game was started with --enable-lua-udp. The
-- server's was; the joining client's was not.
local function fresh(name, st, world, udp)
  local p = { name = name, h = {}, sent = {}, inbox = {}, storage = st, tick = 0,
              recvd = 0, world = world, app = nil, udp = udp }
  use(p)
  package.loaded["control"] = nil
  package.loaded["fk_module"] = nil
  package.loaded["fk_abi"] = nil
  package.loaded["fk_api_gen"] = nil
  require("control")
  return p
end

-- The companion's port, which is what this arm's guest names as Config.Port. A
-- datagram from anywhere else is another mod's companion and the guest library
-- drops it on the source port before reading a header byte.
local OURS, FOREIGN = @@PORT@@, 40000

-- The three REQ payloads, per arm, because the handler is per arm: the wiring
-- fixtures echo whatever they are sent and the demo mods parse "set KEY INT"
-- and answer "err" to anything else, which would leave the guest above the
-- library untouched.
local REQ1, REQ2, REQ3 = "@@REQ1@@", "@@REQ2@@", "@@REQ3@@"

-- Each entry is {bytes} or {bytes, source_port}; the port defaults to the
-- companion's, so only the foreign-port frame has to say anything.
local function tickpeer(p, t, frames)
  use(p)
  p.tick = t
  for i = 1, #frames do
    p.inbox[#p.inbox + 1] = { p = frames[i][1], src = frames[i][2] or OURS }
  end
  p.h[EV_TICK]({ tick = t })
end

-- ---------------------------------------------------------------------------
-- A: the server. A full handshake, and enough traffic that the link is carrying
-- real state -- an epoch, per-channel seq counters, a dedup entry, a heartbeat
-- schedule -- by the time the save is taken.
--
-- HOW IT BOOTS IS THE ARM. Fresh mode is a new map, which is what this harness
-- has always driven. Stale mode is a LOAD of a save written by another build
-- with no on_configuration_changed, which is what every dev rebuild is and what
-- the stamp fold made of every cached map at once -- see bootFresh/bootStale.
-- ---------------------------------------------------------------------------
@@BOOTA@@

tickpeer(A, 1, {})
local hello = A.sent[1]
if not hello or hello:sub(1,2) ~= "FK" or hello:byte(4) ~= 1 then
  -- A bare return rather than os.exit: the sandbox has no os table, so the
  -- original spelling turned this diagnostic into an unrelated Lua error.
  print("JOIN baseline the server never said HELLO -- " .. #A.sent ..
        " frames on the first tick")
  do return end
end
local helloCorr = u32(hello, 17)

-- The NAME the ACK carries is the arm's, and it has to be: every guest in the
-- corpus sets Config.ExpectPeer, so an ACK calling itself anything else is
-- refused, the session never comes up, and the window below would compare two
-- peerless guests to each other.
local PEER = "@@PEER@@"
local EPOCH = 0x51C0FFEE
local ackBody = string.char(1, 1) .. p16(2048) .. p16(16) .. p32(0) .. p32(1) ..
                string.char(0, 0) .. p16(#PEER) .. PEER
tickpeer(A, 2, { { frame(2, 0, 0, EPOCH, 0, helloCorr, 0, 1, ackBody) } })
tickpeer(A, 3, { { frame(4, 0, 2, EPOCH, 1, 0, 0, 1, "cmd") } })
tickpeer(A, 4, { { frame(5, 0, 2, EPOCH, 2, 77, 0, 1, REQ1) } })
for t = 5, 70 do tickpeer(A, t, {}) end

print("INFO event_scratch " .. tostring(require("fk_api_gen").event_scratch))
print("INFO shards " .. #A.storage.fk_mem .. " words-per-shard " ..
      #A.storage.fk_mem[1] .. " memsize " .. tostring(A.storage.fk_memsize))
print("INFO server frames " .. #A.sent)
-- The stamp the joiner is about to download, and the checksum of the heap it is
-- about to download. Both are about the SERVER and neither can be read off the
-- tick comparison below: a server that never republished hands the joiner a
-- frozen heap that both peers then leave alone, so every tick compares
-- identical over two guests that share nothing.
print("INFO stamp " .. tostring(A.storage.fk_build))
local sumBefore = memsum(A.storage.fk_mem)

-- ---------------------------------------------------------------------------
-- B: the joiner. It downloads the save -- a deep copy of storage, because a
-- reference would make the whole comparison vacuous -- rebuilds the module from
-- the same bytes, and runs on_load, which is where fk_mod.lua arms the
-- fk_after_load one-shot. THE SERVER DOES NOT DO ANY OF THIS; it has been
-- running since tick 1.
--
-- Its WORLD is built from the server's for the same reason its storage is: a
-- joining client downloads the game state, and a joiner whose surface still
-- read the default daytime would assemble a different telemetry frame out of an
-- honest guest -- a harness bug wearing a desync's clothes.
-- ---------------------------------------------------------------------------
local B = fresh("B", deepcopy(A.storage), newworld(A.world), false)
if B.h.on_load then B.h.on_load() end
print("JOIN baseline " .. (diffstate(A, B) or "identical"))

-- ---------------------------------------------------------------------------
-- THE JOINED WINDOW: lockstep on the guest's own dynamics AND on inbound
-- traffic, with the memory compared after every dispatch.
--
-- Sixty-five ticks, so it covers the joiner's very first tick (where
-- fk_after_load fires and the server does nothing), a heartbeat window, and the
-- tick-120 telemetry send every guest in the corpus makes -- all of which are
-- guest-state changes on both peers that have to land identically.
--
-- THE DATAGRAMS ARE THE PART THAT WAS MISSING, and what they reach is not the
-- guest library but the HOST: a datagram is delivered from inside the guest's
-- own pump, so the event is a NESTED dispatch and it is fk_mod.lua's LEVEL-2
-- scratch buffer that has to be the same address on both peers. It was not
-- until the buffer caches were mirrored into storage (P12), and the arm that
-- pinned that divergence lived here.
--
-- Six datagrams in three shapes, all replicated identically because an inbound
-- datagram is an InputAction: three plain messages, two correlated requests
-- (which move the guest's dedup and seq state, make it answer, AND -- on the
-- demo arms -- make it write to the world and read the result back), and ONE
-- FRAME FROM A FOREIGN SOURCE PORT. That last one is the attribution leg -- the
-- guest library drops it on the port before reading a header byte, so nothing
-- fkipc owns can be what moves, while the host encodes the event regardless. If
-- this window ever goes red on tick 100 alone, the host is what broke.
--
-- The requests land at 90 and 120, which are also telemetry ticks on the demo
-- arms: a request is delivered from inside Pump, applied later in the same
-- fk_on_tick, and reported by the frame that tick sends -- so the LAST frame of
-- the window carries the effect of the LAST request, which is what the Go side
-- asserts on.
-- ---------------------------------------------------------------------------
local INBOUND = {
  [75]  = { { frame(4, 0, 2, EPOCH, 3, 0,  0, 1, "one") } },
  [76]  = { { frame(4, 0, 2, EPOCH, 4, 0,  0, 1, "two") } },
  [90]  = { { frame(5, 0, 2, EPOCH, 5, 78, 0, 1, REQ2) } },
  [100] = { { frame(4, 0, 2, EPOCH, 99, 0, 0, 1, "not ours"), FOREIGN } },
  [110] = { { frame(4, 0, 2, EPOCH, 6, 0,  0, 1, "three") } },
  [120] = { { frame(5, 0, 2, EPOCH, 7, 79, 0, 1, REQ3) } },
}

A.recvd, B.recvd = 0, 0
A.app, B.app = nil, nil
local want = 0
for t = 71, 135 do
  local f = INBOUND[t] or {}
  want = want + #f
  tickpeer(A, t, f)
  tickpeer(B, t, f)
  local d = diffstate(A, B)
  print("JOIN tick " .. t .. " " .. (d or "identical"))
end
print("INFO inbound " .. A.recvd .. " " .. B.recvd .. " " .. want)
print("INFO app A " .. tostring(A.app))
print("INFO app B " .. tostring(B.app))
print("INFO memsum " .. sumBefore .. " " .. memsum(A.storage.fk_mem))
`
