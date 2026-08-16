package luagen

import (
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// emitGC is emitWith in a chosen GC mode. Both axes are here because the gate
// on the inlined 8-byte store now takes both, and the interesting cases are the
// corners: table mode is where the hole was, and packed mode is where the
// precedent came from.
func emitGC(t *testing.T, wat string, lvl analysis.Level, p PersistMode, g GCMode) string {
	t.Helper()
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	src, err := EmitModuleWith(im, Options{Opt: lvl, Persist: p, GC: g})
	if err != nil {
		t.Fatalf("emit at -opt=%s: %v", lvl, err)
	}
	return src
}

// EVERY STORE SHAPE MARKS ITS PAGE, at every -opt level, in the mode the
// collector runs in.
//
// This is the test agents/gc.md's stage-C gate asks for and stage B owes,
// because stage B is what widened the gate. The rule it enforces is the one
// --persist=packed has been built on since M6 and that the collector's write
// barrier now inherits:
//
//	NOTHING WRITES MEM WITHOUT TESTING MEMDIRTY FIRST.
//
// The stakes went up when the mechanism got a second consumer. Under packed a
// missed mark is a store that never reaches the save -- stale memory after a
// reload. Under the collector it is a live object the sweep reclaims, which is
// a use-after-free inside a lockstep simulation, and the two failures look
// nothing alike from the outside.
//
// It is a TEXT property or it is nothing. No checksum, conformance assertion or
// differential run can see a missing mark: every answer stays right for the
// whole session, and the damage appears later and somewhere else. That is why
// this walks the emitted source rather than running it, and why it covers every
// level rather than the default -- the inlined stores only exist at -opt=3, and
// "we only ship -opt=3" is exactly the reasoning that leaves a hole at 2.
func TestEveryStoreShapeMarksItsPageUnderTheCollector(t *testing.T) {
	// One function per store width, so a failure names the width.
	const wat = `(module (memory 1)
		(func (export "s8")  (param i32 i32) (i32.store8  (local.get 0) (local.get 1)))
		(func (export "s16") (param i32 i32) (i32.store16 (local.get 0) (local.get 1)))
		(func (export "s32") (param i32 i32) (i32.store   (local.get 0) (local.get 1)))
		(func (export "s64") (param i32 i64) (i64.store   (local.get 0) (local.get 1)))
		(func (export "sf32") (param i32 f32) (f32.store  (local.get 0) (local.get 1)))
		(func (export "sf64") (param i32 f64) (f64.store  (local.get 0) (local.get 1)))
		(func (export "sloop") (param i32 i32)
			(local i32)
			(block $b (loop $l
				(br_if $b (i32.ge_u (local.get 2) (local.get 1)))
				(i32.store (i32.add (local.get 0) (i32.shl (local.get 2) (i32.const 2)))
					(local.get 2))
				(local.set 2 (i32.add (local.get 2) (i32.const 1)))
				(br $l))))
		(func (export "l64") (param i32) (result i64) (i64.load (local.get 0))))`

	exports := []string{"s8", "s16", "s32", "s64", "sf32", "sf64", "sloop"}
	for _, lvl := range allLevels {
		src := emitGC(t, wat, lvl, PersistTable, GCCollected)
		for _, name := range exports {
			body := functionBody(src, name)
			for _, line := range strings.Split(body, "\n") {
				if !strings.Contains(line, "MEM[") || !strings.Contains(line, "] = ") {
					continue
				}
				// A direct MEM write is legal only where the same line, or the
				// guard hoisted above the loop, has already marked the span.
				if strings.Contains(line, "MEMDIRTY") {
					continue
				}
				if strings.Contains(body, "MEMPACK.mark") {
					continue // the loop guard's hoisted mark covers the whole span
				}
				t.Errorf("-opt=%s --gc=collected: %s writes MEM with no MEMDIRTY "+
					"test anywhere in the function. Armed while a collection is "+
					"marking, that write is invisible to the barrier, and the "+
					"object it points at is swept while live:\n%s\n%s",
					lvl, name, strings.TrimSpace(line), body)
				break
			}
		}
		// The LOAD is unaffected in every mode -- nothing records a read -- so
		// the -opt=3 expansion must still be there. A gate that also disabled
		// the load would be a silent performance regression with no reason.
		if lvl >= analysis.O3 {
			if !strings.Contains(functionBody(src, "l64"), "S1[t1 + 1]") {
				t.Errorf("-opt=%s --gc=collected: the i64 LOAD stopped inlining; "+
					"only stores are recorded, so only stores are gated", lvl)
			}
		}
	}
}

// The gate, structurally: --gc=collected keeps the 8-byte store out of line in
// table mode, which is the one combination where the hole was.
//
// agents/gc.md found it by scanning a real emitted chunk for a `MEM[...] = ...`
// with no MEMDIRTY test above it and getting exactly nine sites, six of them
// inlined 8-byte stores. Every other writer -- the helpers, the inlined 4-byte
// store, the loop guard's hoisted mark -- already marks in every mode.
func TestCollectedModeKeepsTheEightByteStoreOutOfLine(t *testing.T) {
	for _, lvl := range allLevels {
		src := emitGC(t, f64WAT, lvl, PersistTable, GCCollected)
		for _, name := range []string{"stf", "sti"} {
			if body := functionBody(src, name); strings.Contains(body, "S1[t1 + 1] = ") {
				t.Errorf("-opt=%s table+collected: %s inlines its 8-byte store and "+
					"so writes MEM with no page mark:\n%s", lvl, name, body)
			}
		}
	}
}

// And the control, which is the whole reason this is a compile-time flag: a
// guest that does NOT ask for a collector emits exactly what it emitted before.
//
// agents/gc.md's recommendation was chosen over emitting the mark line into the
// wide store partly on this property. The two were measured indistinguishable,
// and this one has the tiebreaker: the idle cost of the collector's barrier is
// zero rather than small, because an un-armed chunk is byte-for-byte today's.
func TestLeakingModeStillInlinesTheEightByteStore(t *testing.T) {
	for _, lvl := range allLevels {
		if lvl < analysis.O3 {
			continue
		}
		src := emitGC(t, f64WAT, lvl, PersistTable, GCLeaking)
		for _, name := range []string{"stf", "sti"} {
			if body := functionBody(src, name); !strings.Contains(body, "S1[t1 + 1] = ") {
				t.Errorf("-opt=%s table+leaking: %s stopped inlining its 8-byte "+
					"store. The gate is meant to cost a guest that opts IN, and "+
					"nothing at all to one that does not:\n%s", lvl, name, body)
			}
		}
	}
}

// The default is leaking, in both directions: the zero value of the option and
// the string the flag parser produces for an absent flag.
//
// Worth pinning because it is the kind of default that gets flipped by a
// well-meaning change and then ships. --gc=collected caps a guest's heap,
// changes what the emitter inlines and requires an import the guest may not
// have; none of that may happen to somebody who did not ask.
func TestTheGCModeDefaultsToLeaking(t *testing.T) {
	var o Options
	if o.GC != GCLeaking {
		t.Errorf("the zero value of Options.GC is %v, want leaking", o.GC)
	}
	for _, s := range []string{"", "leaking", "none", "off"} {
		m, err := ParseGCMode(s)
		if err != nil {
			t.Errorf("ParseGCMode(%q): %v", s, err)
		} else if m != GCLeaking {
			t.Errorf("ParseGCMode(%q) = %v, want leaking", s, m)
		}
	}
	for _, s := range []string{"collected", "custom"} {
		m, err := ParseGCMode(s)
		if err != nil {
			t.Errorf("ParseGCMode(%q): %v", s, err)
		} else if m != GCCollected {
			t.Errorf("ParseGCMode(%q) = %v, want collected", s, m)
		}
	}
	if _, err := ParseGCMode("conservative"); err == nil {
		t.Error("ParseGCMode accepted \"conservative\"; TinyGo's own collector is " +
			"not what this flag selects, and silently treating it as one would " +
			"emit a chunk gated for a barrier nothing arms")
	}
}

// The collector's half of the page set reaches control.lua, and ONLY under
// --gc=collected.
//
// `persist.gc` is three closures -- arm, disarm, drain -- and they are what the
// pacing handler in runtime/lua/fk_mod.lua drives. Emitting them
// unconditionally would be harmless at runtime and wrong on the property the
// whole design was given a GO on: a guest that does not ask for a collector
// emits a chunk that is byte-for-byte what it emitted before. The same argument
// as TestLeakingModeStillInlinesTheEightByteStore, one surface over.
//
// The drain closure has to capture MEM as an UPVALUE rather than take it as an
// argument, and that is load-bearing rather than stylistic: memory.grow and
// `adopt` both ASSIGN the chunk-local MEM, and Lua gives one upvalue cell per
// local per enclosing scope, so every closure in the chunk sees the assignment.
// A drain that had been handed the table at emit time would write dirtied page
// numbers into the table the guest used to have.
func TestTheCollectorsPageSetSurfaceIsEmittedOnlyWhenAsked(t *testing.T) {
	const wat = `(module (memory 1)
		(func (export "s32") (param i32 i32) (i32.store (local.get 0) (local.get 1))))`

	for _, lvl := range allLevels {
		for _, p := range []PersistMode{PersistTable, PersistPacked, PersistNone} {
			src := emitGC(t, wat, lvl, p, GCCollected)
			for _, want := range []string{
				"gc = {",
				"arm = MEMPACK.gc_arm,",
				"disarm = MEMPACK.gc_disarm,",
				"MEMPACK.gc_drain(MEM, MEMSIZE,",
			} {
				if !strings.Contains(src, want) {
					t.Errorf("-opt=%s --persist=%s --gc=collected: the emitted "+
						"persist surface has no %q. control.lua cannot arm the "+
						"write barrier or read the dirty page set without it, so "+
						"the collector marks with the barrier off",
						lvl, p, want)
				}
			}
			// EVERY persist mode, including `none`. A guest that does not save
			// still collects, and the barrier is not a persistence feature --
			// it only shares a mechanism with one.
		}

		// Scoped to the emitted PERSIST SURFACE and not to the whole chunk,
		// because the prelude is a hand-written file inlined into every chunk in
		// every mode -- MEMPACK.arm, pack, flush and restore are all in a
		// --persist=none chunk today for the same reason. What is generated per
		// guest is the surface, and that is what the flag gates.
		leak := persistSurface(t, emitGC(t, wat, lvl, PersistTable, GCLeaking))
		if strings.Contains(leak, "gc = {") {
			t.Errorf("-opt=%s --gc=leaking: the collector's page-set surface was "+
				"emitted for a guest that did not ask for a collector. The whole "+
				"reason this is a compile-time flag is that an un-armed chunk is "+
				"byte-for-byte today's", lvl)
		}
	}
}

// persistSurface returns the emitted `persist = { ... }` table, which is the
// generated part -- as opposed to the prelude above it, which is a hand-written
// file inlined verbatim into every chunk in every mode.
func persistSurface(t *testing.T, src string) string {
	t.Helper()
	i := strings.Index(src, "  persist = {")
	if i < 0 {
		t.Fatalf("no persist surface in the emitted chunk")
	}
	j := strings.Index(src[i:], "\n  },")
	if j < 0 {
		t.Fatalf("could not find the end of the persist surface")
	}
	return src[i : i+j]
}
