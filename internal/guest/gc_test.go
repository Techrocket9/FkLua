package guest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// The stage-B collector gates, from agents/gc.md.
//
// Every one of them is a DIFFERENTIAL against the same guest source built with
// -gc=leaking, which is the shape TestBothToolchainsAgree already uses for the
// two languages and is what makes these real checks rather than "it did not
// crash". A collector that reclaims something live does not produce an error:
// the memory is still addressable, it is zeroed and handed to somebody else, so
// the only symptom anywhere is a number that moved.

// gcRun compiles a guest at the given -gc and runs a Lua body against it.
//
// The body sees `K`, a table forwarding to the module's exports, and `WORDS()`,
// the authoritative linear-memory size on the Lua side -- which is the number
// this whole feature is measured by, because linear memory never shrinks.
func gcRun(t *testing.T, h *luahost.Host, pkg string, collected bool, body string) string {
	t.Helper()
	out, _ := gcTimed(t, h, pkg, collected, body)
	return out
}

// gcTimed is gcRun plus how long the LUA RUN took -- not the tinygo build, not
// the emit, and not the process launch's own share of anything a caller does
// not want in the number.
//
// It exists because there is no clock in the sandbox: bin/lua52f is patched to
// Factorio's shape and has no `os`, which is deliberate and must stay that way.
// So a duration inside a guest is measured the only way stage A and stage B
// measured one -- by differencing two runs that do the same thing a different
// number of times, from out here.
func gcTimed(t *testing.T, h *luahost.Host, pkg string, collected bool, body string) (string, time.Duration) {
	t.Helper()
	src := gcChunk(t, pkg, collected) + body
	start := time.Now()
	s, err := h.RunString(src)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("running %s (collected=%v): %v\n%s", pkg, collected, err, s)
	}
	return strings.TrimSpace(s), elapsed
}

// gcChunk builds a guest, emits it, and returns the Lua preamble a body runs
// against. Memoised on (package, gc mode): a tinygo build is about a second and
// the stage-C tests drive the same two guests a dozen ways.
var gcChunks sync.Map

func gcChunk(t *testing.T, pkg string, collected bool) string {
	t.Helper()
	key := pkg + "|" + strconv.FormatBool(collected)
	if v, ok := gcChunks.Load(key); ok {
		return v.(string)
	}
	src := gcBuildChunk(t, pkg, collected)
	gcChunks.Store(key, src)
	return src
}

func gcBuildChunk(t *testing.T, pkg string, collected bool) string {
	t.Helper()
	root := repoRoot(t)
	out := filepath.Join(t.TempDir(), "g.wasm")
	build := guest.Build
	if collected {
		build = guest.BuildCollected
	}
	if err := build(filepath.Join(root, "guest", "go"), pkg, out); err != nil {
		t.Fatalf("building %s (collected=%v): %v", pkg, collected, err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	m, err := wasm.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range im.Funcs {
		if f.Unsupported != nil {
			t.Fatalf("collected=%v: function %q did not compile: %v",
				collected, f.Name, f.Unsupported)
		}
	}
	// -opt=3 and --persist=table: the defaults, which is what a guest gets.
	//
	// THE GC MODE HAS TO FOLLOW THE BUILD. Emitting a leaking chunk for a
	// collected guest would inline the 8-byte store past the page mark, which
	// is precisely the hole the emitter gate exists to close -- and no
	// assertion in this file could see it, because the answers stay right for
	// the whole run and the damage is a live object swept later.
	gc := luagen.GCLeaking
	if collected {
		gc = luagen.GCCollected
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{
		Opt: analysis.O3, Persist: luagen.PersistTable, GC: gc})
	if err != nil {
		t.Fatal(err)
	}
	// The log shim records rather than discards, so a differential can compare
	// what the guest SAID and not only what it returned -- most examples return
	// nothing and log everything. The rest of the driver -- K, WORDS, STEP, PACE
	// and the deliberately blind STEPBLIND -- is in gcPreamble, which the Rust half
	// of these gates uses unchanged; see gcrust_test.go. It is ONE text and not two
	// copies because a harness that drove the two toolchains differently would make
	// every cross-language comparison in this package a comparison of harnesses.
	return gcPreamble(t, chunk)
}

func gcFields(t *testing.T, out string) map[string]int {
	t.Helper()
	got := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		for _, kv := range strings.Fields(strings.TrimSpace(line)) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			// Guest LOG lines are in this stream too and are not
			// key=value pairs in any useful sense ("fnv64(fklua)=449d..."),
			// so a value that is not a number is not a field.
			n, err := strconv.Atoi(v)
			if err != nil {
				continue
			}
			got[k] = n
		}
	}
	if len(got) == 0 {
		t.Fatalf("no key=value fields in:\n%s", out)
	}
	return got
}

func needGuest(t *testing.T) *luahost.Host {
	t.Helper()
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	return h
}

// THE ANSWER DOES NOT CHANGE, which is the only gate that can fail silently.
//
// The churn guest runs a fixed number of events in fixed-size batches, with a
// full collection between batches, and the checksum is compared against the
// same guest under -gc=leaking driven the SAME WAY. Batching identically
// matters and cost a wrong result once while this was being built: churn's
// event index restarts at zero in every call, so ten calls of 500 events are
// not one call of 5,000 and the two produce different -- both correct --
// checksums.
//
// The heap assertion is the other half. agents/gc.md's premise is that the
// guest which reaches 16 MiB under -gc=leaking never leaves a fraction of that
// under a collector, and it is stated in LINEAR MEMORY rather than in bytes
// reclaimed because linear memory is what Factorio walks: 0.2 ms of worst tick
// per MiB, size and not usage, and no collection can ever give a doubling back.
func TestCollectedChurnAgreesAndStaysBounded(t *testing.T) {
	h := needGuest(t)
	const events, batch = 20000, 200
	body := fmt.Sprintf(`
local done, sum = 0, 0
while done < %d do
  local n = %d if %d - done < n then n = %d - done end
  sum = K['churn_events'](n)
  done = done + n
  if K['churn_collect'] and K['churn_gc_stat'](9) == 1 then K['churn_collect']() end
end
print(string.format('checksum=%%d words=%%d heap=%%d live=%%d grows=%%d cycles=%%d',
  sum, WORDS(), K['churn_gc_stat'](0), K['churn_gc_stat'](1),
  K['churn_gc_stat'](4), K['churn_gc_stat'](3)))
`, events, batch, events, events)

	leak := gcFields(t, gcRun(t, h, "./examples/churn", false, body))
	coll := gcFields(t, gcRun(t, h, "./examples/churn", true, body))

	if leak["checksum"] != coll["checksum"] {
		t.Errorf("the collector changed the answer: -gc=leaking checksum %d, "+
			"collected %d. A variant that computes a different answer is not a "+
			"faster variant, and here it is not even a variant -- it is a "+
			"collector that reclaimed something live",
			leak["checksum"], coll["checksum"])
	}
	if coll["cycles"] < events/batch-1 {
		t.Fatalf("only %d collections ran over %d events in batches of %d; the "+
			"guest was not actually collecting and nothing below means anything",
			coll["cycles"], events, batch)
	}
	t.Logf("%d events, collecting every %d: linear memory %d words leaking, "+
		"%d collected (%.0fx less); heap %d B, live %d B, %d grows, %d collections",
		events, batch, leak["words"], coll["words"],
		float64(leak["words"])/float64(coll["words"]),
		coll["heap"], coll["live"], coll["grows"], coll["cycles"])

	// The gate is a RATIO rather than an absolute, because the absolute is the
	// collection interval's to choose and the ratio is the collector's.
	if coll["words"]*4 >= leak["words"] {
		t.Errorf("collected linear memory is %d words against -gc=leaking's %d; "+
			"the collector is meant to keep the guest OFF the doubling ladder, "+
			"and a heap that lands within 4x of the leaking one has not",
			coll["words"], leak["words"])
	}
}

// EVERYTHING REACHABLE SURVIVES, and the shapes a conservative non-moving
// collector gets wrong quietly are named one by one.
//
// agents/gc.md's stage-B kill criterion is retention: if the live set the
// collector believes in is more than ~2x what the guest actually has, the heap
// doubles anyway and the feature has bought a barrier and a bitmap for nothing.
// Risk 2 says where to measure it -- "on a guest with a large live set, not on
// churn" -- because the conservative range test gets MORE permissive as the
// heap grows.
func TestTheCollectorKeepsWhatIsReachable(t *testing.T) {
	h := needGuest(t)
	const nodes = 20000
	body := fmt.Sprintf(`
local st = K['torture_stat']
local built = K['torture_build'](%d)
local ip = K['torture_interior'](12345)
local op = K['torture_one_past'](777)
local lg = K['torture_large'](40000)
local before = K['torture_verify']()
K['torture_collect']()
print(string.format('built=%%d before=%%d after=%%d interior=%%d interior_want=%%d large=%%d large_want=%%d one_past=%%d',
  built, before, K['torture_verify'](), K['torture_interior_read'](), ip,
  K['torture_large_read'](), lg, K['torture_one_past_read']()))
print(string.format('kept=%%d believed=%%d liveobj=%%d', K['torture_kept_bytes'](), st(1), st(5)))
K['torture_drop_all']()
K['torture_collect']()
K['torture_collect']()
print(string.format('dropped_live=%%d dropped_obj=%%d cycles=%%d', st(1), st(5), st(3)))
`, nodes)

	leak := gcFields(t, gcRun(t, h, "./examples/gctorture", false, body))
	coll := gcFields(t, gcRun(t, h, "./examples/gctorture", true, body))

	// -gc=leaking reclaims nothing, so its checksums are right by
	// construction. That is what makes it the oracle rather than a second
	// opinion.
	for _, k := range []string{"built", "before", "after", "interior", "large"} {
		if leak[k] != coll[k] {
			t.Errorf("%s: -gc=leaking says %d, collected says %d -- the collector "+
				"reclaimed something that was still reachable", k, leak[k], coll[k])
		}
	}
	if coll["after"] != coll["before"] {
		t.Errorf("the structure changed across a collection: %d before, %d after",
			coll["before"], coll["after"])
	}
	// An INTERIOR pointer is the only reference to that block, and it points
	// into its middle. agents/gc.md section 1 requires this to work: a parked
	// goroutine's asyncifysp is stack+8.
	if coll["interior"] != coll["interior_want"] {
		t.Errorf("a block referenced ONLY through an interior pointer was "+
			"reclaimed: read %d, wrote %d", coll["interior"], coll["interior_want"])
	}
	if coll["large"] != coll["large_want"] {
		t.Errorf("a multi-span object did not survive: read %d, wrote %d",
			coll["large"], coll["large_want"])
	}

	// ONE PAST THE END. This is asserted rather than inherited, which is what
	// agents/gc.md asks for, and the answer is NO: a pointer to the byte after
	// a block does not keep it alive. That is a defensible choice -- accepting
	// them means retaining the object before every genuine base pointer -- but
	// it is the specific reason the wasip1 gate stays shut, because a task's
	// csp is stack+stackSize and nothing else refers to that stack except
	// through the block's base.
	if leak["one_past"] != 1 {
		t.Fatalf("the -gc=leaking control says a one-past-the-end read does not "+
			"see what was written (%d); the probe is broken, not the collector",
			leak["one_past"])
	}
	if coll["one_past"] != 0 {
		t.Errorf("a one-past-the-end pointer retained its block (%d). That is not "+
			"wrong, but it is a CHANGE: this test records that it does not, and "+
			"agents/gc.md's wasip1 gate is argued on that answer", coll["one_past"])
	}

	// The retention gate.
	ratio := float64(coll["believed"]) / float64(coll["kept"])
	t.Logf("%d nodes: the guest holds %d B, the collector believes %d B in %d "+
		"objects -- retention %.3fx; one-past-the-end retains: %v",
		nodes, coll["kept"], coll["believed"], coll["liveobj"], ratio,
		coll["one_past"] == 1)
	if ratio > 2 {
		t.Errorf("conservative retention is %.2fx the real live set, against a "+
			"~2x bar: a heap that over-retains this much doubles regardless and "+
			"the collector has bought nothing", ratio)
	}
	// And the other direction, which a collector that simply retains
	// everything would pass the above on: once every root is gone, essentially
	// nothing should be believed live.
	if coll["dropped_live"] > 4096 {
		t.Errorf("after dropping every root the collector still believes %d B in "+
			"%d objects are live; it is retaining rather than collecting",
			coll["dropped_live"], coll["dropped_obj"])
	}
}

// The whole examples corpus, under the collector.
//
// This is agents/gc.md's stage-B gate (1), and it matters more than it looks:
// churn and gctorture were written for this feature and know it exists, while
// these examples were not and do not. A guest that never mentions the collector
// and behaves differently under it is the failure mode with no other symptom.
//
// Two halves, because the corpus is two kinds of guest. Every wasm-unknown
// example must BUILD with the collector -- which is a real assertion, since
// -gc=custom fails to link unless the seven hooks are provided, and the whole
// claim of the fkgc import being free is that adding it to a guest that does
// not collect changes nothing. The three that run standalone under bin/lua52f
// -- the rest need the Factorio API bound -- are then driven identically under
// both and compared.
//
// examples/goroutine is absent on purpose: it is wasip1, and agents/gc.md
// section 1 gates that combination for stages B to D. Root discovery for a
// parked goroutine is argued there and is untested under a collector, and
// shipping an untested second root-discovery path is how a soundness bug gets
// into a lockstep game.
func TestTheExamplesAgreeUnderTheCollector(t *testing.T) {
	h := needGuest(t)
	root := repoRoot(t)

	// Half one: it builds at all.
	for _, ex := range []string{"api", "array", "callcost", "dict", "grow", "heap", "hello"} {
		out := filepath.Join(t.TempDir(), ex+".wasm")
		if err := guest.BuildCollected(filepath.Join(root, "guest", "go"),
			"./examples/"+ex, out); err != nil {
			t.Errorf("examples/%s does not build with the collector: %v", ex, err)
		}
	}

	// Half two: it behaves the same.
	//
	// A collection between every exported call, which is what agents/gc.md's
	// gate asks for -- not one at the end. Between calls is the only place a
	// collection is allowed to begin (section 1), and it is also where a
	// collector that reclaims something the guest is about to read again would
	// show it.
	body := `
local function collect() if K['fk_gc'] then K['fk_gc']() end end
local oi = K['fk_on_init'] if oi then oi() end
collect()
local ot = K['fk_on_tick']
for tick = 1, 20 do
  if ot then ot(tick) end
  collect()
end
print(LOGS())
print(string.format('words=%d', WORDS()))
`
	for _, ex := range []string{"hello", "grow"} {
		t.Run(ex, func(t *testing.T) {
			leak := gcRun(t, h, "./examples/"+ex, false, body)
			coll := gcRun(t, h, "./examples/"+ex, true, body)
			lw, cw := gcFields(t, leak)["words"], gcFields(t, coll)["words"]
			// The memory SIZE legitimately differs -- that is the feature --
			// so it is dropped before the byte comparison and reported below.
			strip := func(s string) string {
				var keep []string
				for _, l := range strings.Split(s, "\n") {
					if !strings.HasPrefix(strings.TrimSpace(l), "words=") {
						keep = append(keep, l)
					}
				}
				return strings.Join(keep, "\n")
			}
			if strip(leak) != strip(coll) {
				t.Errorf("examples/%s says something different under the collector:\n"+
					"  -gc=leaking:\n%s\n  collected:\n%s", ex, strip(leak), strip(coll))
			}
			t.Logf("%-5s identical output; linear memory %d words leaking, %d collected",
				ex, lw, cw)
		})
	}
}

// The collector's metadata is a documented, bounded, statically reserved cost,
// and it is LINEAR MEMORY -- which agents/guests.md prices at 0.2 ms of
// Factorio worst tick per MiB whether or not anything is using it.
//
// It is asserted rather than logged because it is the kind of number that grows
// by accident: one more bitmap, one deeper mark stack, and a guest that opted
// in to a collector to stay off the doubling ladder has paid a doubling to get
// there.
func TestTheCollectorMetadataFitsItsBudget(t *testing.T) {
	h := needGuest(t)
	out := gcRun(t, h, "./examples/gctorture", true,
		`print(string.format('meta=%d words=%d', K['torture_stat'](8), WORDS()))`)
	f := gcFields(t, out)
	t.Logf("collector metadata: %d B of .bss (%.1f KiB), initial linear memory "+
		"%d words (%.0f KiB)", f["meta"], float64(f["meta"])/1024,
		f["words"], float64(f["words"])*4/1024)
	// 256 KiB covers a 16 MiB heap at a 16-byte granule with room for the span
	// table, the class tables and the mark stack. agents/gc.md budgeted
	// "128 KiB per 16 MiB of heap" for the bitmap alone.
	if f["meta"] > 256*1024 {
		t.Errorf("collector metadata is %d B, over the 256 KiB budget; that is "+
			"%.2f ms of Factorio worst tick a guest pays before it allocates "+
			"anything", f["meta"], float64(f["meta"])/(1024*1024)*0.2)
	}
}
