package luagen

// THE CORRECTNESS COVERAGE FOR SHARDED LINEAR MEMORY ABOVE 4 MiB.
//
// The conformance suite has NONE. Every spec module it converts declares a
// memory of a few pages, so before this file the entire sharded representation
// -- three access forms, a shard vector, a partial last shard, four bulk
// operations that split spans and an 8-byte access that crosses a table
// boundary -- was exercised only inside shard 0, where sharding is a no-op by
// construction. A green suite said nothing at all about it.
//
// THESE ARE HOST-SIDE AND THAT IS LEGITIMATE. They assert ANSWERS, not timings.
// bin/lua52f is wrong about what a large Lua table COSTS -- its array part
// grows to 2^30 and it cannot see Factorio's wall at all -- and it is right
// about every word in it. agents/sharding.md, "Verify the answers under
// bin/lua52f before timing anything in the game", is the rule this file is.
//
// Everything runs at all four opt levels through sameAtEveryLevel, because the
// four levels emit genuinely different access forms: a call to the runtime
// helper below -opt=3, and the static fold, the guard-hoisted form and the
// shard-0 fast path at it. A property proved at one level says nothing about
// another here.

import (
	"fmt"
	"strings"
	"testing"
)

// shardMemWAT is 5 MiB -- 80 pages -- which is THREE shards with the last one
// PARTIAL: 2 MiB, 2 MiB, 1 MiB. The partial last shard is a shape the flat
// representation never had, and half of what follows is about it.
//
// Every access width the emitter lowers differently gets an export, because the
// interesting boundary behaviour is per width: 4-byte aligned accesses provably
// cannot straddle a shard, 8-byte ones can, and sub-word ones straddle freely
// and go byte by byte.
const shardMemWAT = `(module
	(memory 80)
	(func (export "s32") (param i32) (param i32) (i32.store (local.get 0) (local.get 1)))
	(func (export "l32") (param i32) (result i32) (i32.load (local.get 0)))
	(func (export "s64") (param i32) (param i64) (i64.store (local.get 0) (local.get 1)))
	(func (export "l64") (param i32) (result i64) (i64.load (local.get 0)))
	(func (export "s8") (param i32) (param i32) (i32.store8 (local.get 0) (local.get 1)))
	(func (export "l8") (param i32) (result i32) (i32.load8_u (local.get 0)))
	(func (export "s16") (param i32) (param i32) (i32.store16 (local.get 0) (local.get 1)))
	(func (export "l16") (param i32) (result i32) (i32.load16_u (local.get 0)))
	(func (export "sf") (param i32) (param f64) (f64.store (local.get 0) (local.get 1)))
	(func (export "lf") (param i32) (result f64) (f64.load (local.get 0)))
	(func (export "fill") (param i32) (param i32) (param i32)
		(memory.fill (local.get 0) (local.get 1) (local.get 2)))
	(func (export "copy") (param i32) (param i32) (param i32)
		(memory.copy (local.get 0) (local.get 1) (local.get 2)))
	(func (export "size") (result i32) (memory.size))
	(func (export "grow") (param i32) (result i32) (memory.grow (local.get 0))))`

// shardExpr wraps a statement block as the single expression runAt evaluates.
//
// runAt takes an expression because most of what it is asked is one; this file
// asks for a SEQUENCE -- store, then load, then compare -- so each case is an
// immediately-invoked function that returns one string. The string, not a
// number, because a case that checks eight things should say which one moved.
func shardExpr(body string) string {
	return "(function()\nlocal E = M.exports\nlocal out = {}\n" + body +
		"\nreturn table.concat(out, \" \")\nend)()"
}

// The shard geometry, spelled once. SHB is the shard in bytes; the memory is
// three of them with the last a half.
const (
	shb  = 2097152 // bytes per shard
	mem5 = 5242880 // 80 pages
)

// EVERY SHARD, INCLUDING THE LAST PARTIAL ONE, AND BOTH SIDES OF EVERY
// BOUNDARY.
//
// Six words: the first and last word of each of the three shards. The last
// shard's last word is at 5242876, which is 1 MiB into a shard the vector sized
// at 1 MiB -- if mem_grow had rounded the last shard up to 2 MiB the answers
// would be identical and the memory 1 MiB larger, and if it had rounded DOWN
// this is the access that would find nil.
//
// The values are the address itself divided by four, so a value landing in the
// wrong shard is a wrong number rather than a coincidence.
func TestEveryShardIncludingTheLastPartialOneLoadsAndStores(t *testing.T) {
	addrs := []int{0, shb - 4, shb, 2*shb - 4, 2 * shb, mem5 - 4}
	var b strings.Builder
	for _, a := range addrs {
		fmt.Fprintf(&b, "E.s32(%d, %d)\n", a, a/4)
	}
	// Written first and read afterwards, all six, so a store into the wrong
	// shard is caught by the READ of whatever it clobbered as well as by its
	// own.
	for _, a := range addrs {
		fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", a)
	}
	want := "0 524287 524288 1048575 1048576 1310719"
	sameAtEveryLevel(t, shardMemWAT, shardExpr(b.String()), want)
}

// The bytes either side of a boundary, at every sub-word width.
//
// A 4-byte aligned access cannot straddle -- a shard is 524,288 whole words --
// but a BYTE access lands wherever it is told and a 2-byte one straddles freely.
// These are the addresses where the shard index changes between one byte and
// the next: 2097151/2097152 and 4194303/4194304.
func TestBothSidesOfEveryShardBoundary(t *testing.T) {
	var b strings.Builder
	for i, a := range []int{shb - 1, shb, 2*shb - 1, 2 * shb} {
		fmt.Fprintf(&b, "E.s8(%d, %d)\n", a, 0xA0+i)
	}
	// A 16-bit access whose two bytes are in two different shards. It is the
	// narrowest straddle there is and it goes byte by byte, so it proves the
	// per-byte shard select rather than the aligned fast path.
	fmt.Fprintf(&b, "E.s16(%d, 4660)\n", shb-1) // 0x1234
	for _, a := range []int{shb - 1, shb, 2*shb - 1, 2 * shb} {
		fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l8(%d))\n", a)
	}
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l16(%d))\n", shb-1)
	// The 16-bit store overwrote the first two of the four bytes: 0x34 at
	// 2097151 (still shard 0) and 0x12 at 2097152 (shard 1). The other two are
	// untouched.
	want := "52 18 162 163 4660"
	sameAtEveryLevel(t, shardMemWAT, shardExpr(b.String()), want)
}

// THE 8-BYTE STRADDLE: the one correctness shape with no analogue in the flat
// representation.
//
// At offset 2097148 of a shard an aligned 8-byte access has its low word in one
// table and its high word in the NEXT. The flat form could not express the
// problem: one bounds check and two writes into one table cannot half-succeed.
//
// Proved three ways, because "it round-trips" is the weakest of them:
//   - the i64 store and load agree;
//   - the two HALVES are then read back as separate 4-byte loads at 2097148 and
//     2097152, which are provably in different shards, so the halves are shown
//     to have landed on either side of the boundary rather than both on one;
//   - the same for an f64, whose lowering is a different one (f64_to_bits into
//     st_f64 rather than the i64 pair path).
func TestAStraddlingEightByteAccessCrossesTheShardBoundary(t *testing.T) {
	var b strings.Builder
	// 0x99887766_55443322: distinct halves, and neither half is a value the
	// other could be confused with.
	fmt.Fprintf(&b, "E.s64(%d, 1430532898, 2575142758)\n", shb-4)
	fmt.Fprintf(&b, "local lo, hi = E.l64(%d)\n", shb-4)
	b.WriteString("out[#out + 1] = tostring(lo) .. \",\" .. tostring(hi)\n")
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", shb-4)
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", shb)
	// The second boundary too, and with an f64 rather than an i64: 1.5 is
	// 0x3FF8000000000000, so the low word is 0 and the high word is 1073217536.
	fmt.Fprintf(&b, "E.sf(%d, 1.5)\n", 2*shb-4)
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.lf(%d))\n", 2*shb-4)
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", 2*shb-4)
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", 2*shb)
	want := "1430532898,2575142758 1430532898 2575142758 1.5 0 1073217536"
	sameAtEveryLevel(t, shardMemWAT, shardExpr(b.String()), want)
}

// AND THE ONE THAT DECIDES WHETHER THE ORDER IS RIGHT: a straddling 8-byte
// store that is ALSO out of range must trap and leave BOTH shards untouched.
//
// The spec requires an out-of-range store to modify nothing. In a flat memory
// that is a single leading bounds check and there is no way to get it wrong
// once. Under sharding the failure mode is new and it is quiet: write the low
// word, then reach for `mem[s + 1]` for the high word, find nil, and raise a
// Lua error instead of a wasm trap -- with the low word already written.
//
// Reaching the case takes a memory that ENDS on a shard boundary, so the guest
// grows to exactly 6 MiB = three whole shards first. Then 6291452 is a straddle
// offset AND the last four bytes of the memory: an 8-byte store there needs
// bytes past the end, and its high word would be shard 3, which does not exist.
//
// A sentinel goes in first, so "untouched" is checked rather than assumed. And
// the trap has to be the OOB trap, not a Lua indexing error -- an error message
// about a nil value is what this test exists to fail on.
func TestATrappingStraddleTrapsAndLeavesMemoryUntouched(t *testing.T) {
	var b strings.Builder
	// 80 pages + 16 = 96 = 6 MiB exactly, which is three whole shards.
	b.WriteString("E.grow(16)\n")
	const last = 3*shb - 4 // 6291452
	fmt.Fprintf(&b, "E.s32(%d, 305419896)\n", last)
	fmt.Fprintf(&b, "local ok, err = pcall(E.s64, %d, 1, 2)\n", last)
	b.WriteString("out[#out + 1] = tostring(ok)\n")
	b.WriteString("out[#out + 1] = tostring(type(err) == \"table\" and err.fk_trap or err)\n")
	// The sentinel is still there: nothing was half-written.
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", last)
	// And a straddling LOAD one word past the end traps the same way.
	fmt.Fprintf(&b, "local ok2, err2 = pcall(E.l64, %d)\n", last)
	b.WriteString("out[#out + 1] = tostring(ok2)\n")
	b.WriteString("out[#out + 1] = tostring(type(err2) == \"table\" and err2.fk_trap or err2)\n")
	want := "false out of bounds memory access 305419896 false out of bounds memory access"
	sameAtEveryLevel(t, shardMemWAT, shardExpr(b.String()), want)
}

// One word past the end: the ordinary OOB, at the one address where a sharded
// memory could get it wrong by looking up a shard that does not exist.
//
// The last shard is PARTIAL -- 1 MiB of a 2 MiB shard -- so the word one past
// the end is inside a table that exists, at an index that has no value. That is
// the difference from the flat form, where one past the end was simply a nil
// slot too: here a bounds check that trusted the table instead of MEMSIZE would
// read nil and return it as a number.
func TestOneWordPastTheEndTrapsAndWritesNothing(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(&b, "E.s32(%d, 4242)\n", mem5-4)
	fmt.Fprintf(&b, "local ok, err = pcall(E.l32, %d)\n", mem5)
	b.WriteString("out[#out + 1] = tostring(ok)\n")
	b.WriteString("out[#out + 1] = tostring(type(err) == \"table\" and err.fk_trap or err)\n")
	fmt.Fprintf(&b, "local ok2 = pcall(E.s32, %d, 7)\n", mem5)
	b.WriteString("out[#out + 1] = tostring(ok2)\n")
	// The last real word is unchanged, and a byte load one past the end traps
	// too -- the byte path selects its shard separately and could disagree.
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", mem5-4)
	fmt.Fprintf(&b, "out[#out + 1] = tostring(pcall(E.l8, %d))\n", mem5)
	want := "false out of bounds memory access false 4242 false"
	sameAtEveryLevel(t, shardMemWAT, shardExpr(b.String()), want)
}

// memory.fill across TWO shards and across THREE.
//
// The fill splits its word-wise middle at every shard boundary and runs one
// plain loop per piece. What that can get wrong is a piece boundary off by a
// word in either direction, which shows up as an unwritten word at a boundary
// or a written one past the end of the span -- so both are checked, on both
// sides of both boundaries, plus the ragged head and tail.
func TestMemoryFillSpansTwoAndThreeShards(t *testing.T) {
	var b strings.Builder
	// Sentinels either side of what the fill should reach.
	fmt.Fprintf(&b, "E.s32(%d, 0)\n", shb-2100)
	fmt.Fprintf(&b, "E.s32(%d, 0)\n", 2*shb+1100)
	// Two shards, deliberately RAGGED at both ends -- head and tail bytes plus
	// a word middle -- and straddling the 2 MiB boundary.
	fmt.Fprintf(&b, "E.fill(%d, 171, %d)\n", shb-2049, 4098)
	for _, a := range []int{shb - 2050, shb - 2049, shb - 4, shb, shb + 2047, shb + 2048, shb + 2049} {
		fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l8(%d))\n", a)
	}
	// Three shards: from inside shard 0 to inside shard 2, so shard 1 is
	// covered end to end and is one whole piece of its own.
	fmt.Fprintf(&b, "E.fill(%d, 205, %d)\n", shb-1000, shb+2000)
	for _, a := range []int{shb - 1001, shb - 1000, shb, 2*shb - 1, 2 * shb, 2*shb + 999, 2*shb + 1000} {
		fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l8(%d))\n", a)
	}
	// The span is [2095103, 2099200] inclusive -- start + length - 1 -- so the
	// two sentinels are 2095102 below and 2099201 above, and everything named
	// between them is inside it.
	want := strings.Join([]string{
		// two-shard fill: sentinel, then five inside spanning the boundary,
		// then the sentinel above.
		"0", "171", "171", "171", "171", "171", "0",
		// three-shard fill, 0xCD = 205. The byte before is what the first fill
		// left there (0xAB = 171, since 2097152-1001 is inside its span).
		"171", "205", "205", "205", "205", "205", "0",
	}, " ")
	sameAtEveryLevel(t, shardMemWAT, shardExpr(b.String()), want)
}

// memory.copy where SOURCE AND DESTINATION SPLIT DIFFERENTLY.
//
// This is the case a single split would get wrong. A copy's two streams are
// almost never at the same offset in their shards, so a piece is the largest
// run that stays inside one source shard AND one destination shard at once --
// and the destination's boundary and the source's boundary land in different
// places within the same copy.
//
// Four shapes, because they are four different loops in mem_copy: the aligned
// fast path upward, the aligned path overlapping DOWNWARD (memory.copy is
// memmove), the ragged path with a matching source alignment, and the ragged
// path with a SHIFTED source -- which is the one that reads a word ahead of
// where it writes and so has to carry its look-ahead across a piece boundary.
func TestMemoryCopySplitsBothStreamsIndependently(t *testing.T) {
	var b strings.Builder
	// A recognisable source pattern spanning the first boundary: word i holds
	// i, for the 8 words around 2 MiB.
	for i := -4; i < 4; i++ {
		fmt.Fprintf(&b, "E.s32(%d, %d)\n", shb+i*4, 1000+i)
	}
	// 1. Aligned, upward, destination in shard 2 and source across 0/1: the
	//    destination never crosses a boundary and the source does.
	fmt.Fprintf(&b, "E.copy(%d, %d, 32)\n", 2*shb+64, shb-16)
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", 2*shb+64+i*4)
	}
	// 2. Aligned, overlapping DOWNWARD across a boundary. Copying [x, x+32)
	//    to [x+8, x+40) with x just below the boundary makes the destination
	//    cross where the source does not, in the direction that reads ahead of
	//    the write.
	fmt.Fprintf(&b, "E.copy(%d, %d, 32)\n", shb-8, shb-16)
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", shb-8+i*4)
	}
	// 3. Ragged with a SHIFTED source: destination 4-aligned, source at 1 mod
	//    4, across the second boundary on the source side. Read back as bytes,
	//    which is the only honest way to check a shifted copy.
	fmt.Fprintf(&b, "E.s32(%d, 4293844428)\n", 2*shb-4) // 0xFFEEDDCC
	fmt.Fprintf(&b, "E.s32(%d, 1146447479)\n", 2*shb)   // 0x44556677
	fmt.Fprintf(&b, "E.copy(%d, %d, 6)\n", 3*shb/2, 2*shb-3)
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l8(%d))\n", 3*shb/2+i)
	}
	want := strings.Join([]string{
		// 1. the eight words 996..1003 land in order in shard 2
		"996", "997", "998", "999", "1000", "1001", "1002", "1003",
		// 2. the same eight words shifted up two slots: [x+8, x+40) now holds
		//    what [x, x+32) held, so reading from x+8 gives 996..1003 again.
		"996", "997", "998", "999", "1000", "1001", "1002", "1003",
		// 3. bytes 4194301..4194306 of the source, little-endian: 0xDD 0xEE
		//    0xFF are the top three bytes of 0xFFEEDDCC, the last word of
		//    shard 1, and 0x77 0x66 0x55 are the bottom three of 0x44556677,
		//    the first word of shard 2. The copy reads across the boundary and
		//    writes into the middle of shard 1 at a different alignment.
		"221", "238", "255", "119", "102", "85",
	}, " ")
	sameAtEveryLevel(t, shardMemWAT, shardExpr(b.String()), want)
}

// A grow that ADDS a shard, and one that does not.
//
// Two things can go wrong and only one of them is obvious. The obvious one is
// that the new shard is never created and the first access into it finds nil.
// The quiet one is the PARTIAL last shard: a grow from 5 MiB has to finish
// filling the half-empty third shard before it starts a fourth, and a version
// that simply appended whole shards would leave a 1 MiB hole of nils in the
// middle of the memory -- addressable, in range, and reading as nil rather than
// as the zero the spec promises.
func TestAGrowThatAddsAShardAndOneThatDoesNot(t *testing.T) {
	var b strings.Builder
	b.WriteString("out[#out + 1] = tostring(E.size())\n")
	// +16 pages = 6 MiB: fills the partial third shard and adds nothing.
	b.WriteString("out[#out + 1] = tostring(E.grow(16))\n")
	b.WriteString("out[#out + 1] = tostring(E.size())\n")
	// The words the grow filled INSIDE the existing partial shard read as zero,
	// and are writable.
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", mem5)
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", 3*shb-4)
	fmt.Fprintf(&b, "E.s32(%d, 11)\n", 3*shb-4)
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", 3*shb-4)
	// +32 pages = 8 MiB: a whole new fourth shard.
	b.WriteString("out[#out + 1] = tostring(E.grow(32))\n")
	b.WriteString("out[#out + 1] = tostring(E.size())\n")
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", 3*shb)
	fmt.Fprintf(&b, "E.s32(%d, 22)\n", 4*shb-4)
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", 4*shb-4)
	// And the word that was written before either grow is still what it was:
	// a grow appends, it does not rebuild.
	fmt.Fprintf(&b, "out[#out + 1] = tostring(E.l32(%d))\n", 3*shb-4)
	want := "80 80 96 0 0 11 96 128 0 22 11"
	sameAtEveryLevel(t, shardMemWAT, shardExpr(b.String()), want)
}
