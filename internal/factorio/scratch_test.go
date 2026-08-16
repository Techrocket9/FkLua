package factorio

import (
	"strings"
	"testing"
)

// A counting allocator, so a test can assert that the scratch region was used
// rather than inferring it from a timing.
const scratchSetup = `
local allocs = 0
local next_ = 60000
H.bind_alloc(function(n) allocs = allocs + 1
                         local p = next_ next_ = next_ + n return p end,
             function() end)
-- 64 bytes at 8192, which is small enough that a test can overrun it on
-- purpose without writing a kilobyte of Lua.
H.bind_scratch(8192, 64)
local sig1 = { args = {}, rets = { { name = "r0", kind = H.K_STR, at = 0 } } }
local function strAt(p) return M.read_string(IO.ld32(p), IO.ld32(p + 4)) end
`

// A string that fits comes out of the scratch region, and the allocator is
// never called.
//
// This is the whole point of the change: a real fk_alloc is a //go:wasmexport
// whose body is make([]byte, n) compiled to Lua, measured at ~1333 ns against
// the 53 ns the ABI cost test's Lua-closure stub costs.
func TestAStringThatFitsNeverReachesTheAllocator(t *testing.T) {
	h := newABIHarness(t, scratchSetup)
	got := h.run(t, `
print(H.encode_rets(sig1, 1024, "iron-plate"))
print(strAt(1024))
print("allocs " .. allocs)
print("in-region " .. tostring(IO.ld32(1024) >= 8192 and IO.ld32(1024) < 8256))`)
	want := "0\niron-plate\nallocs 0\nin-region true"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A string too long for what is left falls back to the allocator rather than
// truncating or failing.
//
// The fallback is what lets the region be small. Sizing it for the worst case
// would mean reserving guest memory for a blueprint string nobody asks for.
func TestAStringTooLongForTheScratchFallsBackToTheAllocator(t *testing.T) {
	h := newABIHarness(t, scratchSetup)
	got := h.run(t, `
local long = string.rep("x", 100)          -- the region is 64
print(H.encode_rets(sig1, 1024, long))
print(strAt(1024) == long)
print("allocs " .. allocs)
print("in-region " .. tostring(IO.ld32(1024) >= 8192 and IO.ld32(1024) < 8256))`)
	want := "0\ntrue\nallocs 1\nin-region false"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Several strings encoded by one call get their own bytes.
//
// The region is a BUMP pointer rather than a single slot precisely because one
// member can return several strings, and an array or a struct can hold many
// more. A slot would have every one of them pointing at the last.
func TestSeveralStringsInOneEncodeDoNotOverlap(t *testing.T) {
	h := newABIHarness(t, scratchSetup)
	got := h.run(t, `
local sig3 = { args = {}, rets = {
  { name = "a", kind = H.K_STR, at = 0 },
  { name = "b", kind = H.K_STR, at = 8 },
  { name = "c", kind = H.K_STR, at = 16 } } }
print(H.encode_rets(sig3, 1024, "copper", "iron", "steel"))
print(strAt(1024) .. "," .. strAt(1032) .. "," .. strAt(1040))
print("allocs " .. allocs)`)
	want := "0\ncopper,iron,steel\nallocs 0"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// THE RE-ENTRANCY INVARIANT, and the one that would corrupt data silently.
//
// Factorio raises some events synchronously from inside the API call that
// caused them, so the order is: an event's string fields are written into the
// region, the guest handler starts reading them, and the handler makes its own
// host calls before it has finished. A handler reads its fields LAZILY from the
// pointer it was handed rather than copying them out -- that is what made the
// original scratch-buffer bug invisible, and the same is true here.
//
// So a call must reclaim the region only back to where IT started. Resetting to
// zero at the top of encode_rets would write the handler's return values
// straight over the event fields it is still reading: structurally valid bytes
// belonging to something else, which is a desync rather than an error.
//
// The sequence below is that shape with nothing else in it: write an outer
// string, take a mark the way M.call does, let an inner encode run, release
// back to the mark, and check the outer string is still there.
func TestANestedCallDoesNotClobberAStringTheOuterOneIsStillReading(t *testing.T) {
	h := newABIHarness(t, scratchSetup)
	got := h.run(t, `
-- The outer context: an event's string field, written into the region.
print(H.encode_rets(sig1, 1024, "outer-entity"))

-- A nested host call, exactly as M.call brackets one.
local mark = H.scratch_mark()
print(H.encode_rets(sig1, 2048, "inner"))
print("inner " .. strAt(2048))
H.scratch_release(mark)

-- ... and another, which reuses the space the first one had.
print(H.encode_rets(sig1, 2048, "inner2"))
print("inner2 " .. strAt(2048))

-- The outer string must be untouched by both of them.
print("outer " .. strAt(1024))
print("allocs " .. allocs)`)
	want := "0\n0\ninner inner\n0\ninner2 inner2\nouter outer-entity\nallocs 0"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "outer inner") {
		t.Error("the nested call wrote over the outer string: this is the desync case")
	}
}

// Only the OUTERMOST dispatch may reclaim the whole region, and nothing else
// may call scratch_reset.
//
// Asserted here as well as in fk_mod.lua because the two live in different
// files and the rule is easy to lose: a reset anywhere inside a dispatch has
// exactly the effect the test above exists to prevent.
func TestResetReclaimsEverythingAndIsTheOnlyThingThatDoes(t *testing.T) {
	h := newABIHarness(t, scratchSetup)
	got := h.run(t, `
print(H.encode_rets(sig1, 1024, "aaaaaaaaaaaaaaaaaaaa"))   -- 20 of 64
print(H.encode_rets(sig1, 1032, "bbbbbbbbbbbbbbbbbbbb"))   -- 40 of 64
print("distinct " .. tostring(IO.ld32(1024) ~= IO.ld32(1032)))
-- A third would not fit the remaining 24, so it would fall back...
print(H.encode_rets(sig1, 1040, "cccccccccccccccccccccccccccc"))
print("fell back " .. tostring(allocs == 1))
-- ...until the outermost dispatch hands the whole region back.
H.scratch_reset()
print(H.encode_rets(sig1, 1048, "dddddddddddddddddddd"))
print("reused " .. tostring(IO.ld32(1048) == 8192))
print("still one alloc " .. tostring(allocs == 1))`)
	want := "0\n0\ndistinct true\n0\nfell back true\n0\nreused true\nstill one alloc true"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A guest that does not export the region keeps the allocator path exactly as
// it was. The region is an optimisation, and a guest built against an older
// substrate must not lose the ability to return a string.
func TestAGuestWithoutTheRegionStillReturnsStrings(t *testing.T) {
	h := newABIHarness(t, `
local allocs = 0
local next_ = 60000
H.bind_alloc(function(n) allocs = allocs + 1
                         local p = next_ next_ = next_ + n return p end,
             function() end)
local sig1 = { args = {}, rets = { { name = "r0", kind = H.K_STR, at = 0 } } }
local function strAt(p) return M.read_string(IO.ld32(p), IO.ld32(p + 4)) end
`)
	got := h.run(t, `
print(H.encode_rets(sig1, 1024, "iron-plate"))
print(strAt(1024))
print("allocs " .. allocs)`)
	want := "0\niron-plate\nallocs 1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
