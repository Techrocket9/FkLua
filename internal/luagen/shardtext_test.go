package luagen

// THE PROPERTIES SHARDING NEEDS THAT NO BEHAVIOURAL TEST CAN SEE.
//
// agents/sharding.md section 11 lists them second in the order they should
// worry someone, and the reason is always the same shape: a stale reference is
// self-consistently wrong. The guest reads a table nobody else can reach, every
// answer it computes from it agrees with every other, no checksum moves, no
// conformance assertion fails, and the bytes are simply somewhere else. That is
// the same failure class as the dead loop-guard seed and the missed page mark,
// and this repo has been bitten by it twice already. It wants a TEXT property.

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
)

// The persist surface has to move S1 and SHBOUND with MEM and MEMSIZE.
//
// `fk.memory.adopt` REPLACES the whole vector and `MEMPACK.restore` replaces
// the contents and the size. Both are reached from control.lua's load path, and
// either one leaving S1 pointing at the OLD shard 0 gives a guest whose fast
// path reads a table nothing else in the system can reach: stores land in the
// new vector through the slow arm, loads of the same address come back out of
// the old one, and the two disagree silently for the rest of the session.
//
// Asserted on the emitted text, not on behaviour, because a behavioural test
// would have to provoke a swap AND then read through the fast path AND happen
// to pick an address under SHBOUND. The property is simply: every statement
// that assigns MEM or MEMSIZE on this surface assigns S1 and SHBOUND too.
func TestAdoptAndRestoreRebindEveryDerivedMemoryLocal(t *testing.T) {
	const wat = `(module (memory 1)
		(func (export "f") (param i32) (result i32) (i32.load (local.get 0))))`
	for _, mode := range []PersistMode{PersistTable, PersistPacked, PersistNone} {
		src := emitGC(t, wat, analysis.O3, mode, GCLeaking)
		for _, line := range strings.Split(src, "\n") {
			l := strings.TrimSpace(line)
			// Only the persist surface's own closures, which are the only
			// places a whole memory is installed from outside.
			if !strings.HasPrefix(l, "adopt = function") && !strings.HasPrefix(l, "restore = function") {
				continue
			}
			// MEMPACK.memreset is the fill cursor, and it belongs on this
			// list for exactly the same reason the other two do: it is derived
			// state that survives a memory swap and is wrong afterwards. A
			// cursor left above the new size claims words are materialised in a
			// vector that has never seen them, so the next grow SKIPS a fill it
			// owed -- and a load of one of those words is a nil, arriving deep
			// inside guest code a long way from the grow that caused it.
			for _, name := range []string{"SHBOUND =", "S1 =", "MEMPACK.memreset("} {
				if !strings.Contains(l, name) {
					t.Errorf("--persist=%s: this installs a memory without moving %s with it:\n  %s\n"+
						"A stale S1, SHBOUND or fill cursor is a guest reading a table "+
						"nobody else can reach, or words nobody ever created.",
						mode, strings.TrimSuffix(strings.TrimSuffix(name, " ="), "("), l)
				}
			}
		}
	}
}

// And NOTHING ELSE at chunk scope holds a shard.
//
// S1 is the only chunk-level binding of a shard table, and that is what makes
// the rule above finite: one name to move, in two places. A second one -- an
// `S2` for the guest's second shard, say -- would be a second thing to forget,
// and it would be forgotten, because the reason to add it is a fast path that
// by construction almost never runs.
//
// The emitted MEM[k] forms are fine and are what this allows: they read the
// vector at the point of use, so they cannot be stale.
func TestS1IsTheOnlyChunkLevelShardBinding(t *testing.T) {
	src := emitAt(t, `(module (memory 1)
		(func (export "f") (param i32) (result i32) (i32.load (local.get 0))))`, analysis.O3)
	bind := regexp.MustCompile(`^local (\w+) = MEM\[`)
	for _, line := range strings.Split(src, "\n") {
		if m := bind.FindStringSubmatch(line); m != nil && m[1] != "S1" {
			t.Errorf("a second chunk-level shard binding %q: %s\n"+
				"Only S1 may be one, because only S1 is moved by adopt and restore.",
				m[1], line)
		}
	}
}

// A page can never straddle a shard boundary, which is why the whole dirty-page
// set survived sharding untouched.
//
// A page is 4 KiB and a shard is 2 MiB. Both are powers of two and the page is
// the smaller, so pages nest exactly: 512 per shard, always aligned. That is
// what lets MEMPACK.mark, DPLO/DPHI, every writer's mark call and the two-
// compare fast path all stay byte-for-byte what they were, with only
// pack_page/unpack_page translating an index. The dirty-page set is also the
// collector's write barrier, and that consumer is unaffected for the same
// reason.
//
// Arithmetic rather than behavioural, because the property is about the two
// constants and there is no input that exercises it -- it either holds for
// every page or for none.
func TestAPageNeverStraddlesAShardBoundary(t *testing.T) {
	const pageBytes = 4096 // MEMPACK's PAGEW * 4
	if shardBytes%pageBytes != 0 {
		t.Fatalf("a shard is %d bytes and a page is %d: pages do not nest, so a "+
			"page can straddle a shard boundary and pack_page cannot address it "+
			"with one shard", shardBytes, pageBytes)
	}
	if per := shardBytes / pageBytes; per != 512 {
		t.Errorf("%d pages per shard, want 512 -- pack_page's `p %% 512` and "+
			"`(p - p %% 512) / 512` are that number written twice", per)
	}
	// And a shard is a whole number of WORDS, which is the separate property
	// that makes a 4-byte aligned access unable to straddle one.
	if shardBytes%4 != 0 || shardWords*4 != shardBytes {
		t.Errorf("a shard is %d bytes and %d words: a 4-byte aligned access "+
			"could straddle one, and every else-arm in the emitter assumes it "+
			"cannot", shardBytes, shardWords)
	}
	// Half the wall, exactly. Anything at or above 2^20 keys stops being an
	// array in Factorio, which is the whole reason this representation exists.
	if shardWords != 1<<19 {
		t.Errorf("a shard is %d words; 2^19 is chosen so that a shard can never "+
			"reach 2^20 keys however the memory grows", shardWords)
	}
}

// THE CHUNK-LOCAL BUDGET, LANDED.
//
// agents/codegen.md's budget is the scarcest thing a generated chunk has and
// sharding spends from it: S1 and SHBOUND are two new column-zero names, paid
// for by MEMMAX -- a compile-time constant with one reader -- coming out. Net
// one.
//
// TestPromotionLeavesTheMarginItPromises already pins the margin for a chunk
// with GLOBALS and no memory. This pins the other axis: the floor a chunk with
// MEMORY starts from, so that a prelude or an emitter that grows by a name
// moves a number here rather than moving the cliff for somebody's guest.
func TestAMemoryCostsTheChunkTheNamesShardingSaysItDoes(t *testing.T) {
	const nomem = `(module (func (export "f") (result i32) (i32.const 1)))`
	const mem = `(module (memory 1)
		(func (export "f") (param i32) (result i32) (i32.load (local.get 0))))`
	got := countChunkLocals(emitAt(t, mem, analysis.O3)) -
		countChunkLocals(emitAt(t, nomem, analysis.O3))
	// MEM, MEMSIZE, SHBOUND, S1. Promotion absorbs its own share, so the
	// difference is only what emitModuleState declares.
	const want = 4
	if got != want {
		t.Errorf("a memory costs the chunk %d locals, want %d (MEM, MEMSIZE, "+
			"SHBOUND, S1). MEMMAX is deliberately NOT among them -- it is a "+
			"compile-time constant whose one reader is the memory.grow "+
			"lowering, and its slot is what SHBOUND spends. Update "+
			"agents/codegen.md's budget section with this number.", got, want)
	}
}
