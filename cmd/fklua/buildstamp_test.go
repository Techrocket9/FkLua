package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// stampRE reads the build stamp back out of a packaged mod. The emitter writes
// it as one line of the persistence surface (internal/luagen, `build = %s`), so
// this is the artefact a save actually records rather than a value recomputed
// here -- a test that called buildID itself would pass over a packager that
// never used it.
var stampRE = regexp.MustCompile(`build = "([^"]*)"`)

// packageAtPin packages one guest against one API pin and hands back the
// directory, the stamp in it and the generated chunk.
//
// Everything varies through --api= and nothing else: the same input file, the
// same flags, the same output root. So any difference between two calls is a
// difference the pin caused.
func packageAtPin(t *testing.T, guestPath, pin, tag string) (dir, stamp, chunk string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), tag)
	if err := runMod([]string{guestPath, "--api=" + pin,
		"--name", "stamp-mod", "--version", "0.1.0", "-o", out}); err != nil {
		t.Fatalf("packaging against %s: %v", pin, err)
	}
	dir = filepath.Join(out, "stamp-mod_0.1.0")
	b, err := os.ReadFile(filepath.Join(dir, factorio.GeneratedModuleFile))
	if err != nil {
		t.Fatal(err)
	}
	chunk = string(b)
	m := stampRE.FindStringSubmatch(chunk)
	if m == nil {
		t.Fatalf("no build stamp in the packaged chunk; `fklua mod` stopped "+
			"stamping one, so same_build() can never be true and every load "+
			"discards the heap:\n%s", firstLines(chunk, 40))
	}
	if m[1] == "" {
		t.Fatal("the packaged build stamp is empty, which never matches a " +
			"stamped save -- every load takes the rebuild path")
	}
	return dir, m[1], chunk
}

func firstLines(s string, n int) string {
	ls := strings.Split(s, "\n")
	if len(ls) > n {
		ls = ls[:n]
	}
	return strings.Join(ls, "\n")
}

// pinnedCallingGuest is apipin_test's callingGuest at a stable path, so the
// SAME FILE can be packaged more than once -- which is the whole experiment
// here. callingGuest writes into a fresh t.TempDir() each call, and two
// byte-identical files at two paths would leave "did the bytes move?" as an
// alternative explanation for a stamp that moved.
func pinnedCallingGuest(t *testing.T, id int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "guest.wat")
	src := fmt.Sprintf(`(module
  (import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
  (memory 1)
  (func (export "fk_on_tick") (param $tick i32)
    (i32.store (i32.const 0) (i32.add (i32.load (i32.const 0)) (i32.const 1)))
    (if (i32.load (i32.const 64))
      (then (drop (call $call (i32.const 1) (i32.const %d)
                              (i32.const 0) (i32.const 128)))))))`, id)
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// THE API PIN IS PART OF THE BUILD STAMP, and until this it was not part of it
// at all.
//
// The stamp was sha256 over the wasm alone, so ONE wasm packaged against TWO
// --api pins produced two mods with one identity -- and same_build() therefore
// adopted a heap written by the first into the second. That is unsound as a
// CLASS rather than in one place: member, event and define ids are dense sorted
// indices over a version's own set, so a pin change shifts them wholesale, and
// the package's pin-derived facts reach into the HEAP. API.event_scratch comes
// out of the packaged event table and is the size a cached buffer in the heap
// was allocated at (P12's size guard closes that one symptom, and is kept for
// the case this cannot reach -- fk_migrate_adopt hands over another build's
// heap deliberately); a define id the guest resolves once and caches is a
// per-build number sitting in the same heap.
//
// Four assertions, and the last two are what keep the first two honest:
//
//	the two pins produce DIFFERENT stamps                -- the fix
//	one pin twice produces the SAME stamp                -- nothing
//	                                                        nondeterministic
//	                                                        was folded in
//	the two chunks differ ONLY in the stamp line         -- the pin reaches
//	                                                        the stamp and
//	                                                        nothing else in
//	                                                        the chunk, so the
//	                                                        stamp is the whole
//	                                                        of what a load can
//	                                                        notice
//	the two MEMBER TABLES differ                         -- ...while the
//	                                                        package really does
//	                                                        carry pin-derived
//	                                                        facts, which is why
//	                                                        noticing matters
func TestTheAPIPinIsPartOfTheBuildStamp(t *testing.T) {
	other := otherAPIVersion(t)
	id, _, _ := firstDivergentMember(t, other)
	def := factorio.DefaultAPIVersion

	dir := t.TempDir()
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	back := chdir(t, dir)
	defer back()

	// ONE file, packaged three times. The guest bytes never move.
	guest := pinnedCallingGuest(t, id)
	dirA, stampA, chunkA := packageAtPin(t, guest, def, "a")
	dirB, stampB, chunkB := packageAtPin(t, guest, other, "b")
	_, stampA2, chunkA2 := packageAtPin(t, guest, def, "a2")

	if stampA == stampB {
		t.Errorf("ONE WASM PACKAGED AGAINST TWO API PINS SHARES A BUILD STAMP "+
			"(%s at both %s and %s). same_build() is a string comparison on this "+
			"value, so a save written by the %s package is ADOPTED by the %s one "+
			"-- with the member, event and define ids underneath it assigned over "+
			"a different set, and a cached buffer in the heap allocated at the "+
			"other pin's event_scratch.", stampA, def, other, def, other)
	}
	if stampA != stampA2 {
		t.Errorf("THE STAMP IS NOT STABLE: the same wasm at the same pin (%s) "+
			"stamped %s and then %s. Something nondeterministic is folded into "+
			"it, so every repackage of an unchanged mod discards its users' "+
			"guest state.", def, stampA, stampA2)
	}
	if chunkA != chunkA2 {
		t.Error("two packages of one wasm at one pin produced different chunks, " +
			"so the stamp is not the only thing that could be unstable")
	}

	// The chunk is pin-INDEPENDENT apart from the stamp: the emitter never sees
	// an API version. So the whole of what a load can notice about the pin is
	// this one line, which is exactly why it has to be in it.
	if diff := differingLines(chunkA, chunkB); len(diff) != 1 ||
		!strings.Contains(diff[0], "build = ") {
		t.Errorf("the two chunks differ in %d line(s) rather than in the stamp "+
			"alone: %q. If the pin reaches the chunk somewhere else, this test "+
			"is no longer attributing the stamp change to the pin.",
			len(diff), diff)
	}

	// ...and the package really does carry pin-derived facts. Without this the
	// fold above would be conservatism with nothing behind it.
	tableA, err := os.ReadFile(filepath.Join(dirA, factorio.APIFile))
	if err != nil {
		t.Fatal(err)
	}
	tableB, err := os.ReadFile(filepath.Join(dirB, factorio.APIFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(tableA) == string(tableB) {
		t.Errorf("the packaged member tables for %s and %s are identical, so "+
			"this guest cannot distinguish the pins and the test proves nothing "+
			"about why the stamp has to", def, other)
	}
}

// differingLines reports the lines of a that are not the corresponding lines of
// b. Line-wise rather than a byte diff because the two chunks here are the same
// emitter output over the same module and differ, if at all, in place.
func differingLines(a, b string) []string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for i := range la {
		if i >= len(lb) {
			out = append(out, la[i])
			continue
		}
		if la[i] != lb[i] {
			out = append(out, strings.TrimSpace(la[i]))
		}
	}
	for i := len(la); i < len(lb); i++ {
		out = append(out, lb[i])
	}
	return out
}

// crossPin packages one guest against two API pins and drives the five-session
// script over both, handing back the parsed legs and the two stamps.
//
// Shared by the two tests below because the expensive half -- resolving a second
// cached description, finding a member the two pins number differently, and
// running `fklua mod` twice -- is identical for both, and because the two tests
// are two readings of ONE run: whether the rebuild path is taken at all, and
// whether it is reached when Factorio does not raise on_configuration_changed.
func crossPin(t *testing.T) (legs map[string]map[string]string, stampA, stampB, other, out string) {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	other = otherAPIVersion(t)
	id, _, _ := firstDivergentMember(t, other)

	dir := t.TempDir()
	t.Setenv("FKLUA_API_DIR", filepath.Join(moduleRoot(t), "api"))
	back := chdir(t, dir)
	defer back()

	guest := pinnedCallingGuest(t, id)
	dirA, stampA, _ := packageAtPin(t, guest, factorio.DefaultAPIVersion, "a")
	dirB, stampB, _ := packageAtPin(t, guest, other, "b")
	if stampA == stampB {
		t.Fatal("the two packages share a stamp, so this test cannot say " +
			"anything about what a load does across them")
	}

	out, err = h.RunString(fmt.Sprintf(crossPinScript,
		filepath.Join(dirA, "?.lua"), filepath.Join(dirB, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	legs = map[string]map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(strings.TrimSpace(l))
		if len(f) < 2 {
			continue
		}
		m := map[string]string{}
		for _, kv := range f[1:] {
			if i := strings.IndexByte(kv, '='); i > 0 {
				m[kv[:i]] = kv[i+1:]
			}
		}
		legs[f[0]] = m
	}
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		if legs[name] == nil {
			t.Fatalf("the script never reached leg %s:\n%s", name, out)
		}
		t.Logf("%s %v", name, legs[name])
	}
	return legs, stampA, stampB, other, out
}

// A LOAD ACROSS TWO API PINS TAKES THE REBUILD PATH, through the real
// control.lua -- which is the consequence the stamp exists to produce and the
// half a stamp comparison in Go cannot establish.
//
// Three of the script's sessions, with only `storage` crossing between them,
// exactly as Factorio carries a save:
//
//	1  a new map on the package built at the default pin. Five ticks.
//	2  a load of that save by THE SAME package. The control: the heap is
//	   adopted and the counter keeps going, which is what makes leg 3 mean
//	   something rather than being a harness that never adopts anything.
//	3  a load of the SAME save by the package built at the other pin, WITH
//	   on_configuration_changed -- i.e. a repackage that also moved the mod's
//	   version. The heap must be discarded and the counter must restart -- and
//	   the guest exports neither migrate hook, so the discard is also logged,
//	   naming both stamps.
//
// Legs 4 and 5 are the same repackage with the version left alone, and they are
// the test below.
//
// The two packages come from one .wat file and differ in the --api flag alone.
func TestALoadAcrossTwoAPIPinsTakesTheRebuildPath(t *testing.T) {
	legs, stampA, stampB, other, out := crossPin(t)

	if legs["one"]["ticks"] != "5" || legs["one"]["build"] != stampA {
		t.Fatalf("leg one did not establish the save: %v (stamp %s)",
			legs["one"], stampA)
	}
	// THE CONTROL. Without it, leg three passing would be consistent with a
	// harness that never adopts anything.
	if legs["two"]["ticks"] != "8" {
		t.Fatalf("A SAME-PIN LOAD DID NOT ADOPT (ticks=%s, want 8), so leg three "+
			"cannot distinguish the rebuild path from a harness that reloads from "+
			"zero every time: %v", legs["two"]["ticks"], legs["two"])
	}
	if legs["two"]["logged"] != "0" {
		t.Errorf("a same-pin load logged a rebuild: %v", legs["two"])
	}

	if legs["three"]["ticks"] != "3" {
		t.Errorf("A LOAD ACROSS TWO API PINS ADOPTED THE OTHER PIN'S HEAP "+
			"(ticks=%s, want 3 -- a fresh heap counting only this session). The "+
			"package built at %s ran on a heap laid out by the package built at "+
			"%s, whose member, event and define ids are indices over a different "+
			"version's set.", legs["three"]["ticks"], other,
			factorio.DefaultAPIVersion)
	}
	if legs["three"]["build"] != stampB {
		t.Errorf("leg three did not republish its own stamp (%s, want %s), so "+
			"the next load would take the rebuild path again, forever",
			legs["three"]["build"], stampB)
	}
	// The discard is not silent, and the message is the only place an author
	// ever sees the two stamps -- which is what makes a cross-pin repackage
	// diagnosable rather than merely safe.
	if legs["three"]["logged"] != "1" {
		t.Errorf("the cross-pin rebuild was not logged: %v", legs["three"])
	}
	if !strings.Contains(out, stampA) || !strings.Contains(out, stampB) {
		t.Errorf("the rebuild message does not name both build ids (%s -> %s), "+
			"so an author who repackaged against a new pin is told their state "+
			"was reset and not what reset it:\n%s", stampA, stampB, out)
	}
}

// A REBUILD THAT KEEPS THE MOD'S VERSION IS STILL A REBUILD, AND FACTORIO WILL
// NOT TELL THE MOD SO.
//
// on_configuration_changed fires when the mod SET changes -- for one mod, when
// its VERSION moves. A build stamp moves for a great deal less than that: a dev
// rebuild, a --gc or --persist change, a repackage against another --api pin.
// Every one of those is leg four here, and until 2026-08-07 the whole path was
// invisible because every harness in this repo called on_config() on every load.
//
// What the unfixed runtime does with leg four, measured before the fix:
//
//	four  ticks=5  build=<stampA>  logged=0
//	five  ticks=5  build=<stampA>  logged=0
//
// Read those two rows carefully, because they are worse than a reset. `ticks=5`
// is the save reporting the FIRST session's counter after a session that ran
// three of its own: state_load declined, so storage.fk_mem is still the heap
// build A wrote, while the guest ran on the fresh one _initialize built --
// two unrelated tables, and every write the guest made reached neither the save
// nor the CRC. `build=<stampA>` is the stamp that did not match still sitting
// there, so leg five declines identically and so does every load after it,
// forever. And `logged=0` is the guest never being told: the message and the
// fk_migrate dispatch both live in the hook that did not fire.
//
// ON A MULTIPLAYER JOIN THAT IS A DESYNC, which is how it was found. The server
// declines and runs on happily; a client joins, downloads the same stale stamp,
// declines identically, and starts a tick-0 heap against a server at tick 1250.
// The join half is TestAJoiningPeerStaysByteIdenticalToTheServer's stale arm.
//
// Leg five is the half that says the save was left CONSISTENT rather than merely
// handled once -- an ordinary same-build load that adopts, on the state leg four
// wrote.
func TestASameVersionRebuildIsStillHandled(t *testing.T) {
	legs, stampA, stampB, other, out := crossPin(t)

	// The harness has to be doing the thing first: leg four must really be the
	// SAME repackage leg three is, minus the hook.
	if legs["three"]["build"] != stampB {
		t.Fatalf("leg three did not take the rebuild path, so leg four cannot be "+
			"read as the same load without the hook: %v", legs["three"])
	}

	if legs["four"]["logged"] != "1" {
		t.Errorf("A SAME-VERSION REBUILD TOLD THE AUTHOR NOTHING (logged=%s). "+
			"Factorio raises on_configuration_changed for a VERSION change and a "+
			"dev rebuild keeps the version, so the discard notice -- and the "+
			"fk_migrate dispatch beside it -- never happened. The guest that "+
			"rebuilds from the world on a rebuild is never told to: %v",
			legs["four"]["logged"], legs["four"])
	}
	if legs["four"]["build"] != stampB {
		t.Errorf("A DECLINED LOAD LEFT THE SAVE CARRYING THE STAMP IT DECLINED "+
			"(build=%s, want %s). Nothing republished it, because the only thing "+
			"that ever did was on_configuration_changed -- so this save now says "+
			"it was written by %s while holding a heap %s is running on, every "+
			"later load declines again, and a joining client reads the same stale "+
			"stamp and rebuilds a tick-0 heap against a server that has been "+
			"running for twenty minutes.",
			legs["four"]["build"], stampB, stampA, stampB)
	}
	if legs["four"]["ticks"] != "3" {
		t.Errorf("THE SAVE IS NOT TRACKING THE GUEST (ticks=%s, want 3 -- a fresh "+
			"heap counting only this session's three). %s is what the FIRST "+
			"session left in storage.fk_mem: the decline never republished the "+
			"mirror, so `storage` holds build %s's heap while the guest runs on a "+
			"different table entirely, and its writes reach neither the save nor "+
			"the multiplayer CRC.", legs["four"]["ticks"], legs["four"]["ticks"],
			stampA)
	}

	// LEG FIVE IS THE CONSISTENCY HALF. One handling is not enough if the state
	// it leaves behind declines again next time.
	if legs["five"]["ticks"] != "6" {
		t.Errorf("THE NEXT LOAD DECLINED AGAIN (ticks=%s, want 6 -- leg four's "+
			"three plus three more). A load that handles the rebuild has to leave "+
			"a save an ordinary load can adopt; this one is still self-"+
			"inconsistent, so a guest packaged at %s resets its users' state on "+
			"every single load for the rest of the mod's life.",
			legs["five"]["ticks"], other)
	}
	if legs["five"]["logged"] != "0" {
		t.Errorf("an ordinary load of leg four's save logged a rebuild (%v), so "+
			"the stamp leg four wrote is still not this build's", legs["five"])
	}
	if !strings.Contains(out, "this mod was rebuilt") {
		t.Errorf("the log line that tells an author their state was reset has "+
			"changed its opening clause, which is downstream API surface (see "+
			"agents/guests.md, A LOG LINE IS API SURFACE):\n%s", out)
	}
}

// Three sessions of one mod in one interpreter, with only `storage` crossing.
//
// The dir argument is what models a repackage: a load re-executes control.lua,
// so pointing package.path at the OTHER package and clearing all four
// package.loaded entries is genuinely the other mod running over the first
// one's save. All four, because a package with a member table has fk_abi and
// fk_api_gen behind control.lua as well as fk_module, and a stale one of those
// would be the previous package still in the room.
const crossPinScript = `local dirA, dirB = %q, %q
defines = { events = { on_tick = 1 } }
game = {}

logged = {}
function log(s) logged[#logged + 1] = s end

local function deepcopy(v)
  if type(v) ~= "table" then return v end
  local o = {}
  for k, x in pairs(v) do o[deepcopy(k)] = deepcopy(x) end
  return o
end

-- cfg says whether Factorio raises on_configuration_changed for this load, and
-- it is a PARAMETER rather than an always because it is not always. The hook
-- fires when the mod SET changes -- which for one mod means its VERSION moving.
-- A build stamp moves for a great deal less: a dev rebuild, a --gc or --persist
-- change, a repackage against another --api pin. Passing true unconditionally is
-- what this harness did until 2026-08-07, and it is why the whole same-version
-- rebuild path went untested through two milestones.
local function session(saved, ticks, dir, cfg)
  local handlers = {}
  script = {
    mod_name = "stamp-mod",
    on_init = function(f) handlers.on_init = f end,
    on_load = function(f) handlers.on_load = f end,
    on_configuration_changed = function(f) handlers.on_config = f end,
    on_event = function(ev, f) handlers[ev] = f end,
  }
  storage = saved and deepcopy(saved) or {}
  logged = {}
  package.path = dir
  package.loaded["control"] = nil
  package.loaded["fk_module"] = nil
  package.loaded["fk_abi"] = nil
  package.loaded["fk_api_gen"] = nil
  require("control")

  if saved then
    if handlers.on_load then handlers.on_load() end
    if cfg and handlers.on_config then handlers.on_config() end
  else
    if handlers.on_init then handlers.on_init() end
  end
  for tick = 1, ticks do
    local f = handlers[defines.events.on_tick]
    if f then f({ tick = tick }) end
  end
  return storage
end

local function report(tag)
  print(tag .. " ticks=" .. tostring(storage.fk_mem[1][1]) ..
        " build=" .. tostring(storage.fk_build) ..
        " logged=" .. tostring(#logged))
  for _, s in ipairs(logged) do print("LOG " .. s) end
end

local save = session(nil, 5, dirA, false)
report("one")

session(save, 3, dirA, true)
report("two")

session(save, 3, dirB, true)
report("three")

-- THE SAME CROSS-PIN LOAD WITH NO on_configuration_changed, which is what a
-- repackage that keeps the mod's version actually looks like to the engine.
local four = session(save, 3, dirB, false)
report("four")

-- ...and the load after it, which is the half that says the save was left
-- CONSISTENT rather than merely handled once.
session(four, 3, dirB, false)
report("five")
`
