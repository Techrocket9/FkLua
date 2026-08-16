package factorio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
	luart "github.com/Techrocket9/fklua/runtime"
)

// A module that exists only for its linear memory: the marshalling tests need a
// REAL MEM table with the real bounds checks, not a stand-in, because the whole
// question is whether the two ends agree about bytes.
const memWAT = `(module (memory 1)
	(func (export "f") (result i32) (i32.const 0)))`

// runMarshal instantiates that module, binds its memio into the ABI, and runs
// the script with `H` and `IO` in scope.
func runMarshal(t *testing.T, script string) string {
	t.Helper()
	return runMarshalWithFile(t, "", "", script)
}

// runMarshalWithFile is runMarshal with one extra module written alongside the
// ABI, so a test can `require` generated output the way a packaged mod does.
func runMarshalWithFile(t *testing.T, name, content, script string) string {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	m, err := wasm.DecodeWAT(memWAT)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fk_abi.lua"), []byte(luart.ABI()), 0o644); err != nil {
		t.Fatal(err)
	}
	if name != "" {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := "package.path = " + luaQuote(filepath.Join(dir, "?.lua")) + "\n" +
		"local H = require(\"fk_abi\")\n" +
		"local M = (function(...)\n" + chunk + "\nend)({})\n" +
		"local IO = M.memio\n" +
		"H.bind_memory(IO)\n" +
		"H.bind_read_string(M.read_string)\n" +
		"H.bind_globals({})\n" + script
	out, err := h.RunString(src)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return strings.TrimSpace(out)
}

// The cross-language contract. Go computes the offsets and Lua reads them, and
// nothing at build time can check that the two agree -- so this does, by writing
// bytes at Go's offsets and asking Lua what it sees.
func TestGoAndLuaAgreeOnStructLayout(t *testing.T) {
	// Deliberately awkward: a byte before a double forces padding, and a string
	// aligns to 4 rather than 8 even though it is 8 bytes wide.
	kinds := []Kind{KindU8, KindF64, KindI32, KindBool, KindString, KindU16}
	b, err := Layout(kinds)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 8, 16, 20, 24, 32}
	for i, f := range b.Fields {
		if f.Offset != want[i] {
			t.Errorf("field %d (%v) at %d, want %d", i, f.Kind, f.Offset, want[i])
		}
	}
	if b.Size != 40 || b.Align != 8 {
		t.Errorf("block size %d align %d, want 40/8", b.Size, b.Align)
	}

	// Now prove Lua reads the same bytes back. Values are written through the
	// SAME memio the ABI marshals through, at Go's offsets.
	got := runMarshal(t, fmt.Sprintf(`
local sig = { args = %s, rets = {} }
local base = 256
IO.st8(base + 0, 200)
IO.stf64(base + 8, -2.5)
IO.st32(base + 16, 4294967295)          -- -1 as an i32 under Invariant A
IO.st8(base + 20, 1)
-- a string is (ptr, len); put the bytes somewhere else in memory
local sp = 512
local s = "handle"
for i = 1, #s do IO.st8(sp + i - 1, string.byte(s, i)) end
IO.st32(base + 24, sp)
IO.st32(base + 28, #s)
IO.st16(base + 32, 65535)

local ok, a, b2, c, d = H.decode_args(sig, base)
print(a)
print(b2)
print(c)
print(tostring(d))
local _, _, _, _, _, e, f = H.decode_args(sig, base)
print(e)
print(f)
`, b.LuaTable()))

	wantOut := "200\n-2.5\n-1\ntrue\nhandle\n65535"
	if got != wantOut {
		t.Errorf("got:\n%s\nwant:\n%s", got, wantOut)
	}
}

// Invariant A reaches the ABI: an i32 crosses as an UNSIGNED double, so every
// signed width needs an explicit fold at the boundary. Getting this wrong is a
// wrong number rather than a crash, which is the kind of bug that ships.
func TestSignedFieldsAreFoldedAtTheBoundary(t *testing.T) {
	got := runMarshal(t, `
local function one(kind, write)
  local sig = { args = { {kind=kind, at=0} }, rets = {} }
  write(1024)
  local _, v = H.decode_args(sig, 1024)
  return v
end
print(one(H.K_I8,  function(a) IO.st8(a, 255) end))          -- -1
print(one(H.K_U8,  function(a) IO.st8(a, 255) end))          -- 255
print(one(H.K_I16, function(a) IO.st16(a, 65535) end))       -- -1
print(one(H.K_U16, function(a) IO.st16(a, 65535) end))       -- 65535
print(one(H.K_I32, function(a) IO.st32(a, 2147483648) end))  -- -2^31
print(one(H.K_U32, function(a) IO.st32(a, 2147483648) end))  -- 2^31
`)
	want := "-1\n255\n-1\n65535\n-2147483648\n2147483648"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The whole path: a guest writes arguments into its own memory, calls, and reads
// results back out. This is fk.call, and it is the first point at which a guest
// could actually reach the Factorio API.
func TestCallRoundTripsThroughGuestMemory(t *testing.T) {
	args, err := Layout([]Kind{KindI32, KindF64})
	if err != nil {
		t.Fatal(err)
	}
	rets, err := Layout([]Kind{KindF64, KindBool})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
local surface = {
  valid = true,
  scale = function(n, by) return n * by, n > 0 end,
}
H.bind_members({
  [1] = { kind = H.CALL, name = "scale",
          sig = { args = %s, rets = %s } },
})
local h = H.transient(surface)
local argp, retp = 2048, 3072
IO.st32(argp + %d, 4294967289)     -- -7
IO.stf64(argp + %d, 1.5)
print("status " .. H.call(h, 1, argp, retp))
print("product " .. IO.ldf64(retp + %d))
print("positive " .. tostring(IO.ld8(retp + %d) ~= 0))
`, args.LuaTable(), rets.LuaTable(),
		args.Fields[0].Offset, args.Fields[1].Offset,
		rets.Fields[0].Offset, rets.Fields[1].Offset))

	want := "status 0\nproduct -10.5\npositive false"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A LuaObject returned by the API becomes a TRANSIENT handle, not a persistent
// one. That is the default that makes the leak class impossible: a guest which
// takes the handle and drops it costs nothing.
func TestAReturnedObjectComesBackAsATransientHandle(t *testing.T) {
	rets, err := Layout([]Kind{KindHandle})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
local child = { valid = true, tag = "child" }
H.bind_members({
  [1] = { kind = H.CALL, name = "spawn",
          sig = { args = {}, rets = %s } },
})
local parent = { valid = true, spawn = function() return child end }
local retp = 4096
H.call(H.transient(parent), 1, 0, retp)
local h = IO.ld32(retp)
print(h >= 1073741824 and "transient" or "PERSISTENT")
print((H.get(h)).tag)
H.clear_transient()
print(select(2, H.get(h)) == H.ERR_BAD_HANDLE and "released with the dispatch" or "LEAKED")
`, rets.LuaTable()))

	want := "transient\nchild\nreleased with the dispatch"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The kind numbers are a wire contract between Go and Lua that no compiler
// checks. This reads them out of the ABI source and compares, so drifting one
// side fails here instead of decoding a double as a boolean in someone's game.
func TestKindNumbersMatchTheLuaABI(t *testing.T) {
	src := luart.ABI()
	// Constants are written two to a line: `M.K_I8, M.K_U8   = 1, 2`.
	lua := map[string]int{}
	for _, line := range strings.Split(src, "\n") {
		if !strings.HasPrefix(line, "M.K_") {
			continue
		}
		lhs, rhs, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if i := strings.Index(rhs, "--"); i >= 0 {
			rhs = rhs[:i]
		}
		names := strings.Split(lhs, ",")
		vals := strings.Split(rhs, ",")
		if len(names) != len(vals) {
			t.Fatalf("cannot parse ABI constant line: %q", line)
		}
		for i := range names {
			var v int
			if _, err := fmt.Sscan(strings.TrimSpace(vals[i]), &v); err != nil {
				t.Fatalf("cannot parse value in %q: %v", line, err)
			}
			lua[strings.TrimPrefix(strings.TrimSpace(names[i]), "M.")] = v
		}
	}

	want := map[string]Kind{
		"K_I8": KindI8, "K_U8": KindU8, "K_I16": KindI16, "K_U16": KindU16,
		"K_I32": KindI32, "K_U32": KindU32, "K_F32": KindF32, "K_F64": KindF64,
		"K_BOOL": KindBool, "K_STR": KindString, "K_HANDLE": KindHandle,
		"K_U64": KindU64, "K_STRUCT": KindStruct,
		"K_ARRAY": KindArray, "K_DICT": KindDict, "K_DYN": KindDyn,
	}
	if len(lua) != len(want) {
		t.Errorf("ABI declares %d kinds, Go has %d -- one side gained a kind "+
			"the other does not know about", len(lua), len(want))
	}
	for name, k := range want {
		got, ok := lua[name]
		if !ok {
			t.Errorf("%s is missing from fk_abi.lua", name)
			continue
		}
		if got != int(k) {
			t.Errorf("%s = %d in Lua, %d in Go", name, got, int(k))
		}
	}
}

// Widths and alignments have to agree too: Go places the fields, Lua reads
// them, and a disagreement about either silently shifts everything after it.
//
// KindStruct is deliberately outside the loop: a struct's size is a property of
// its own field list, not of the kind, so neither side carries a number for it.
func TestGoAndLuaAgreeOnWidthAndAlignment(t *testing.T) {
	var b strings.Builder
	b.WriteString("local out = {}\n")
	for k := KindI8; k <= KindU64; k++ {
		fmt.Fprintf(&b, "out[#out+1] = H.field_width(%d) .. \"/\" .. H.field_align(%d)\n",
			int(k), int(k))
	}
	b.WriteString("print(table.concat(out, \" \"))\n")

	var want []string
	for k := KindI8; k <= KindU64; k++ {
		want = append(want, fmt.Sprintf("%d/%d", k.Size(), k.Align()))
	}
	if got := runMarshal(t, b.String()); got != strings.Join(want, " ") {
		t.Errorf("width/align disagree:\ngot:  %s\nwant: %s", got, strings.Join(want, " "))
	}
}

// A string crossing OUT is the one shape that cannot work without the guest's
// allocator: the bytes need somewhere in guest memory to live, and only the
// guest owns that address space.
func TestAReturnedStringIsAllocatedInGuestMemory(t *testing.T) {
	rets, err := Layout([]Kind{KindString})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
-- A bump allocator standing in for the guest's. Records what it handed out so
-- the test can see whether anything leaked.
local next_, handed = 8192, {}
local function alloc(n) local p = next_ next_ = next_ + n handed[p] = n return p end
local function free(p) handed[p] = nil end
H.bind_alloc(alloc, free)
H.bind_members({
  [1] = { kind = H.CALL, name = "describe",
          sig = { args = {}, rets = %s } },
})
local obj = { valid = true, describe = function() return "iron-chest" end }
local retp = 4096
print("status " .. H.call(H.transient(obj), 1, 0, retp))
local p, n = IO.ld32(retp), IO.ld32(retp + 4)
print("len " .. n)
print("text " .. M.read_string(p, n))
print("allocated " .. tostring(handed[p]))
`, rets.LuaTable()))

	want := "status 0\nlen 10\ntext iron-chest\nallocated 10"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// With no allocator bound, a string return has to FAIL rather than invent a
// pointer. A made-up address lands in the middle of whatever the guest had
// there, and the corruption surfaces nowhere near the call.
func TestAStringReturnWithoutAnAllocatorIsRefused(t *testing.T) {
	rets, err := Layout([]Kind{KindString})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
H.bind_members({
  [1] = { kind = H.CALL, name = "describe", sig = { args = {}, rets = %s } },
})
local obj = { valid = true, describe = function() return "text" end }
print(H.call(H.transient(obj), 1, 0, 4096) == H.ERR_BAD_ARGS and "refused" or "INVENTED A POINTER")
`, rets.LuaTable()))
	if got != "refused" {
		t.Errorf("got %q, want %q", got, "refused")
	}
}

// A later field failing must undo the allocations already made. Otherwise the
// first string is owned by nobody -- the host has forgotten it and the guest
// never saw the pointer -- and it leaks for the life of the session.
func TestAFailedEncodeFreesWhatItAlreadyAllocated(t *testing.T) {
	rets, err := Layout([]Kind{KindString, KindString})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
local live, freed = 0, 0
-- The second allocation fails, which is what drives the undo.
local calls = 0
local function alloc(n)
  calls = calls + 1
  if calls == 2 then return 0 end
  live = live + 1
  return 8192
end
local function free(p) freed = freed + 1 end
H.bind_alloc(alloc, free)
H.bind_members({
  [1] = { kind = H.CALL, name = "two", sig = { args = {}, rets = %s } },
})
local obj = { valid = true, two = function() return "first", "second" end }
print(H.call(H.transient(obj), 1, 0, 4096) == H.ERR_NO_SPACE and "out of space" or "WRONG STATUS")
print("allocated " .. live .. ", freed " .. freed)
`, rets.LuaTable()))

	want := "out of space\nallocated 1, freed 1"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(the first allocation has to be undone)", got, want)
	}
}

// An empty string and a nil both cross as (0, 0) and cost no allocation. The
// API returns nil for an absent optional constantly, so allocating for it would
// be a per-call cost paid for nothing.
func TestEmptyAndNilStringsCostNoAllocation(t *testing.T) {
	rets, err := Layout([]Kind{KindString})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
local allocs = 0
H.bind_alloc(function(n) allocs = allocs + 1 return 8192 end, function() end)
H.bind_members({
  [1] = { kind = H.CALL, name = "empty", sig = { args = {}, rets = %s } },
  [2] = { kind = H.CALL, name = "none",  sig = { args = {}, rets = %s } },
})
local obj = { valid = true, empty = function() return "" end, none = function() return nil end }
local h = H.transient(obj)
H.call(h, 1, 0, 4096)
print("empty ptr=" .. IO.ld32(4096) .. " len=" .. IO.ld32(4100))
H.call(h, 2, 0, 4096)
print("nil   ptr=" .. IO.ld32(4096) .. " len=" .. IO.ld32(4100))
print("allocations " .. allocs)
`, rets.LuaTable(), rets.LuaTable()))

	want := "empty ptr=0 len=0\nnil   ptr=0 len=0\nallocations 0"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The write is bounds-checked over the WHOLE span, once. Per-byte checking
// would leave a half-written string behind when it tripped, and the spec's rule
// for an out-of-range store is that memory is not modified at all.
func TestAStringWritePastTheEndTrapsWithoutWritingAnything(t *testing.T) {
	got := runMarshal(t, `
local size = IO.size()
IO.st8(size - 4, 42)                       -- a byte we can check survived
local ok, err = pcall(function() IO.wstr(size - 4, "way too long for this") end)
print(ok and "NO TRAP" or "trapped")
print("byte still " .. IO.ld8(size - 4))
`)
	if want := "trapped\nbyte still 42"; got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Tier 1: a named-field table crossing in both directions, nested, with
// optional fields present and absent. Events and table-shaped concepts are both
// this shape and between them are about 90% of the traffic.
func TestStructRoundTripsWithNestingAndOptionals(t *testing.T) {
	// Modelled on what a real create_entity argument table looks like: a name, a
	// nested position, an optional force, an optional nested bounding box.
	spec := []FieldSpec{
		{Name: "name", Kind: KindString},
		{Name: "position", Kind: KindStruct, Struct: []FieldSpec{
			{Name: "x", Kind: KindF64},
			{Name: "y", Kind: KindF64},
		}},
		{Name: "force", Kind: KindU32, Optional: true},
		{Name: "fast_replace", Kind: KindBool, Optional: true},
	}
	b, err := LayoutStruct(spec)
	if err != nil {
		t.Fatal(err)
	}

	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
local base = 2048
-- Write the bytes the way a guest would, then read them as the ABI does.
local sp = 6000
local nm = "iron-chest"
IO.wstr(sp, nm)
local function fieldAt(n) for _, f in ipairs(fields) do if f.name == n then return f end end end
local nf = fieldAt("name")
IO.st32(base + nf.at, sp)
IO.st32(base + nf.at + 4, #nm)
local pf = fieldAt("position")
IO.stf64(base + pf.at + pf.fields[1].at, 12.5)
IO.stf64(base + pf.at + pf.fields[2].at, -3.25)
local ff = fieldAt("force")
IO.st8(base + ff.has, 1)
IO.st32(base + ff.at, 7)
local rf = fieldAt("fast_replace")
IO.st8(base + rf.has, 0)          -- absent
IO.st32(base + rf.at, 1)          -- and stale bytes underneath it

local t = H.read_struct(fields, base)
print(t.name)
print(t.position.x .. "," .. t.position.y)
print("force " .. tostring(t.force))
print("fast_replace " .. tostring(t.fast_replace))
`, b.LuaTable()))

	// The absent optional must come back nil even though the value bytes under
	// it hold a perfectly plausible 1.
	want := "iron-chest\n12.5,-3.25\nforce 7\nfast_replace nil"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// An absent optional is OMITTED from the table, not defaulted. Factorio
// distinguishes "absent" from "present and false" throughout -- absent means
// leave it alone, present-false means turn it off -- so a default would change
// what the call does.
func TestAnAbsentOptionalIsOmittedNotDefaulted(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{{Name: "enabled", Kind: KindBool, Optional: true}})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
local f = fields[1]
IO.st8(1024 + f.has, 0)
IO.st8(1024 + f.at, 0)
local absent = H.read_struct(fields, 1024)
IO.st8(2048 + f.has, 1)
IO.st8(2048 + f.at, 0)
local present = H.read_struct(fields, 2048)
local n = 0
for _ in pairs(absent) do n = n + 1 end
print("absent has " .. n .. " keys, enabled=" .. tostring(absent.enabled))
print("present false: enabled=" .. tostring(present.enabled))
`, b.LuaTable()))

	want := "absent has 0 keys, enabled=nil\npresent false: enabled=false"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The write direction: a table the API returned becomes bytes the guest reads,
// with presence flags set from which keys are actually there.
func TestAStructReturnWritesPresenceFlags(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{
		{Name: "count", Kind: KindU32},
		{Name: "label", Kind: KindString, Optional: true},
		{Name: "ratio", Kind: KindF64, Optional: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	rets, err := Layout([]Kind{KindI32})
	if err != nil {
		t.Fatal(err)
	}
	_ = rets
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
H.bind_alloc(function(n) return 9000 end, function() end)
-- ratio is absent, label present.
local st = H.write_struct(fields, 3072, { count = 42, label = "hi" })
print("status " .. st)
local function fieldAt(n) for _, f in ipairs(fields) do if f.name == n then return f end end end
print("count " .. IO.ld32(3072 + fieldAt("count").at))
print("label present " .. IO.ld8(3072 + fieldAt("label").has) ..
      " text " .. M.read_string(IO.ld32(3072 + fieldAt("label").at),
                                IO.ld32(3072 + fieldAt("label").at + 4)))
print("ratio present " .. IO.ld8(3072 + fieldAt("ratio").has))
-- And it round-trips back through the reader.
local back = H.read_struct(fields, 3072)
print(back.count .. " " .. back.label .. " " .. tostring(back.ratio))
`, b.LuaTable()))

	want := "status 0\ncount 42\nlabel present 1 text hi\nratio present 0\n42 hi nil"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Nested struct layout has to match C: the inner block aligns to its own
// widest member, and the outer size accounts for it.
func TestNestedStructAlignment(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{
		{Name: "flag", Kind: KindBool},
		{Name: "pos", Kind: KindStruct, Struct: []FieldSpec{
			{Name: "x", Kind: KindF64},
			{Name: "y", Kind: KindF64},
		}},
		{Name: "tag", Kind: KindU8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.Fields[0].Offset != 0 {
		t.Errorf("flag at %d, want 0", b.Fields[0].Offset)
	}
	// The nested block aligns to 8, so it starts at 8, not 1.
	if b.Fields[1].Offset != 8 || b.Fields[1].Align != 8 || b.Fields[1].Size != 16 {
		t.Errorf("pos at %d size %d align %d, want 8/16/8",
			b.Fields[1].Offset, b.Fields[1].Size, b.Fields[1].Align)
	}
	if b.Fields[2].Offset != 24 {
		t.Errorf("tag at %d, want 24", b.Fields[2].Offset)
	}
	// Padded to the outer alignment of 8.
	if b.Size != 32 || b.Align != 8 {
		t.Errorf("block size %d align %d, want 32/8", b.Size, b.Align)
	}
}

// An optional field costs a presence byte AHEAD of its value, which is what a
// Rust Option<T> under repr(C) lays out. Pinned so the generator and the codec
// cannot drift on where it sits.
func TestAnOptionalFieldPlacesItsPresenceByteFirst(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{{Name: "v", Kind: KindF64, Optional: true}})
	if err != nil {
		t.Fatal(err)
	}
	f := b.Fields[0]
	if f.HasOffset != 0 || f.Offset != 8 {
		t.Errorf("presence at %d, value at %d; want 0 and 8", f.HasOffset, f.Offset)
	}
	if b.Size != 16 {
		t.Errorf("size %d, want 16", b.Size)
	}
	// A mandatory field carries no presence byte at all.
	m, _ := LayoutStruct([]FieldSpec{{Name: "v", Kind: KindF64}})
	if m.Fields[0].HasOffset != -1 || m.Size != 8 {
		t.Errorf("mandatory field: has=%d size=%d, want -1 and 8",
			m.Fields[0].HasOffset, m.Size)
	}
}

// Arrays are (ptr, count) with elements out of line, for the same reason a
// string is: how many there are is not known until the value exists.
func TestArrayRoundTrip(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{
		{Name: "ids", Kind: KindArray, Elem: &FieldSpec{Kind: KindU32}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.Fields[0].Stride != 4 {
		t.Errorf("u32 stride = %d, want 4", b.Fields[0].Stride)
	}
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
H.bind_alloc(function(n) return 8192 end, function() end)
print("status " .. H.write_struct(fields, 1024, { ids = {10, 20, 30} }))
print("count " .. IO.ld32(1024 + fields[1].at + 4))
local back = H.read_struct(fields, 1024)
print(#back.ids .. ": " .. table.concat(back.ids, ","))
-- An empty array and a nil both cross as (0,0) and allocate nothing.
H.write_struct(fields, 2048, { ids = {} })
print("empty ptr " .. IO.ld32(2048 + fields[1].at) .. " count " .. IO.ld32(2048 + fields[1].at + 4))
print("empty reads back as " .. #H.read_struct(fields, 2048).ids)
`, b.LuaTable()))

	want := "status 0\ncount 3\n3: 10,20,30\nempty ptr 0 count 0\nempty reads back as 0"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// An array of STRUCTS strides by the element's padded size. Using the sum of
// the field widths instead would read every element after the first from the
// wrong place -- and the first would look right, which is the worst way for
// this to be wrong.
func TestArrayOfStructsStridesByPaddedSize(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{
		{Name: "points", Kind: KindArray, Elem: &FieldSpec{
			Kind: KindStruct, Struct: []FieldSpec{
				{Name: "x", Kind: KindF64},
				{Name: "tag", Kind: KindU8},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 8 for the double, 1 for the byte, padded to 16 -- not 9.
	if got := b.Fields[0].Stride; got != 16 {
		t.Fatalf("stride = %d, want 16 (padded, not the 9 the fields sum to)", got)
	}
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
H.bind_alloc(function(n) return 8192 end, function() end)
H.write_struct(fields, 1024, { points = {
  { x = 1.5, tag = 7 }, { x = -2.5, tag = 9 }, { x = 100.25, tag = 11 },
}})
local back = H.read_struct(fields, 1024).points
local out = {}
for i = 1, #back do out[i] = back[i].x .. "/" .. back[i].tag end
print(table.concat(out, " "))
`, b.LuaTable()))

	if want := "1.5/7 -2.5/9 100.25/11"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A dictionary is an array of key/value PAIRS. Sharing the walk with arrays is
// not just less code: a dict of structs works without anyone writing that case.
func TestDictionaryRoundTrip(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{
		{Name: "counts", Kind: KindDict,
			Key:  &FieldSpec{Kind: KindString},
			Elem: &FieldSpec{Kind: KindU32}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
local next_ = 8192
H.bind_alloc(function(n) local p = next_ next_ = next_ + n + 8 return p end, function() end)
print("status " .. H.write_struct(fields, 1024, { counts = { ["iron-plate"] = 5, ["copper-cable"] = 12 } }))
local back = H.read_struct(fields, 1024).counts
-- Sorted so the assertion does not depend on pairs order, which nothing promises.
local keys = {}
for k in pairs(back) do keys[#keys+1] = k end
table.sort(keys)
local out = {}
for _, k in ipairs(keys) do out[#out+1] = k .. "=" .. back[k] end
print(table.concat(out, " "))
`, b.LuaTable()))

	if want := "status 0\ncopper-cable=12 iron-plate=5"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Nesting composes without special cases: an array inside a struct inside an
// array. This is the shape the recursive read_value/write_value exist for.
func TestAggregatesNestArbitrarily(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{
		{Name: "groups", Kind: KindArray, Elem: &FieldSpec{
			Kind: KindStruct, Struct: []FieldSpec{
				{Name: "label", Kind: KindString},
				{Name: "values", Kind: KindArray, Elem: &FieldSpec{Kind: KindI32}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
local next_ = 8192
H.bind_alloc(function(n) local p = next_ next_ = next_ + n + 8 return p end, function() end)
print("status " .. H.write_struct(fields, 1024, { groups = {
  { label = "a", values = {1, -2, 3} },
  { label = "bb", values = {} },
}}))
local back = H.read_struct(fields, 1024).groups
print(#back)
print(back[1].label .. ":" .. table.concat(back[1].values, ","))
print(back[2].label .. ":" .. #back[2].values)
`, b.LuaTable()))

	want := "status 0\n2\na:1,-2,3\nbb:0"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// An element failing mid-array frees the block rather than leaking it. Same
// rule encode_rets follows for strings, for the same reason: the guest never
// saw the pointer, so nobody else can free it.
func TestAFailedArrayElementFreesTheBlock(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{
		{Name: "names", Kind: KindArray, Elem: &FieldSpec{Kind: KindString}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
local calls, freed = 0, 0
H.bind_alloc(function(n)
  calls = calls + 1
  if calls == 3 then return 0 end     -- the block, then "a", then fail on "b"
  return 8192 + calls * 64
end, function(p) freed = freed + 1 end)
print(H.write_struct(fields, 1024, { names = {"a", "b"} }) == H.ERR_NO_SPACE and "out of space" or "WRONG")
print("freed " .. freed)
`, b.LuaTable()))

	if want := "out of space\nfreed 1"; got != want {
		t.Errorf("got %q, want %q (the array block has to be released)", got, want)
	}
}

// Tier 2: one tagged codec instead of 93 generated union types. The tag says
// what is actually there, which is what carries a structural union and a
// recursive type alike -- and tolerates version skew, since it describes the
// value rather than what the schema said it would be.
func TestDynamicValuesRoundTrip(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{{Name: "v", Kind: KindDyn}})
	if err != nil {
		t.Fatal(err)
	}
	if b.Size != 16 || b.Align != 8 {
		t.Fatalf("a dynamic value is %d/%d, want 16/8", b.Size, b.Align)
	}
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
local next_ = 8192
H.bind_alloc(function(n) local p = next_ next_ = next_ + n + 16 return p end, function() end)

local function trip(v)
  local st = H.write_struct(fields, 1024, { v = v })
  if st ~= H.OK then return "STATUS " .. st end
  return H.read_struct(fields, 1024).v
end

print(tostring(trip(nil)))
print(tostring(trip(true)))
print(tostring(trip(-2.5)))
print(tostring(trip("hello")))
print(tostring(trip("")))
local arr = trip({1, "two", false})
print(#arr .. ":" .. tostring(arr[1]) .. "," .. arr[2] .. "," .. tostring(arr[3]))
local m = trip({ a = 1, b = "x" })
print(tostring(m.a) .. tostring(m.b))
`, b.LuaTable()))

	want := "nil\ntrue\n-2.5\nhello\n\n3:1,two,false\n1x"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

// LocalisedString is the shape tier 2 exists for: defined in terms of itself,
// so no fixed layout holds it. {"item-name.iron-plate", {"", 1, true}} nests a
// list inside a list, which is exactly how the game writes them.
func TestARecursiveValueSurvives(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{{Name: "caption", Kind: KindDyn}})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
local next_ = 8192
H.bind_alloc(function(n) local p = next_ next_ = next_ + n + 16 return p end, function() end)
H.write_struct(fields, 1024, { caption = { "item-name.iron-plate", { "", 1, true } } })
local c = H.read_struct(fields, 1024).caption
print(c[1])
print(#c[2] .. ": [" .. c[2][1] .. "] " .. tostring(c[2][2]) .. " " .. tostring(c[2][3]))
`, b.LuaTable()))

	if want := "item-name.iron-plate\n3: [] 1 true"; got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

// A LuaObject inside a dynamic value becomes a transient handle, like anywhere
// else. Telling one from a plain table cannot be done by reading a key -- a key
// a LuaObject lacks RAISES -- so the probe is guarded, which tier 2 can afford
// and the tier-1 path could not.
func TestADynamicLuaObjectBecomesAHandle(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{{Name: "v", Kind: KindDyn}})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
H.bind_alloc(function(n) return 8192 end, function() end)
-- Shaped like a LuaObject: object_name present, and every other key raises.
local obj = setmetatable({}, { __index = function(_, k)
  if k == "object_name" then return "LuaEntity" end
  error("LuaEntity doesn't contain key " .. tostring(k))
end })
H.write_struct(fields, 1024, { v = obj })
print(H.read_struct(fields, 1024).v == obj and "same object" or "LOST")
-- A plain table with no object_name is data, not an object, and the probe on it
-- must not raise either.
H.write_struct(fields, 2048, { v = { 7 } })
print(H.read_struct(fields, 2048).v[1])
`, b.LuaTable()))

	if want := "same object\n7"; got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

// A DICTIONARY KEYED BY A TIER-2 VALUE crosses, and it crosses at the offsets
// the generated Go decoder reads.
//
// This is the shape `game.surfaces`, `game.players` and `game.forces` return --
// a dictionary whose key is "an index or a name", which is a union and
// therefore K_DYN. The Lua side never had a gap here (read_value routes K_DICT
// into the same walk an array uses, and a K_DYN key is just write_value again),
// so what needs proving is the CONTRACT: Go places the key at 0 and the value
// at the key's PADDED size, and a decoder that used the key's width instead
// would read the value out of the middle of the tag.
//
// The assertion is therefore on the placement as well as the values. A tier-2
// value is 16 bytes, so a (dyn, handle) pair strides by 24 -- not by 20.
func TestADictionaryKeyedByADynamicValueCrosses(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{
		{Name: "surfaces", Kind: KindDict,
			Key:  &FieldSpec{Kind: KindDyn},
			Elem: &FieldSpec{Kind: KindU32}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := b.Fields[0]
	if f.Key.Offset != 0 || f.Elem.Offset != 16 || f.Stride != 24 {
		t.Errorf("pair layout is key@%d val@%d stride %d, want 0/16/24 -- the "+
			"generated decoder reads exactly these numbers",
			f.Key.Offset, f.Elem.Offset, f.Stride)
	}
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
local next_ = 8192
H.bind_alloc(function(n) local p = next_ next_ = next_ + n + 8 return p end, function() end)
print("status " .. H.write_struct(fields, 1024, { surfaces = { [1] = 11, ["nauvis"] = 22 } }))
local back = H.read_struct(fields, 1024).surfaces
local keys = {}
for k in pairs(back) do keys[#keys+1] = tostring(k) end
table.sort(keys)
local out = {}
for _, k in ipairs(keys) do
  local v = back[k] or back[tonumber(k)]
  out[#out+1] = k .. "=" .. v
end
print(table.concat(out, " "))
`, b.LuaTable()))

	if want := "status 0\n1=11 nauvis=22"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
