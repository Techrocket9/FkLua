package factorio

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// The M7 gate: the generated dispatcher exercised against at least one real
// member of EVERY class.
//
// The other dispatch tests pick two members by name and prove the chain works
// for those. That is a chain test, not a coverage one -- 156 classes go through
// the same generated table, and a class whose members all fail to resolve would
// look exactly like a class nobody tested.
//
// One member per class, chosen by shape rather than by name so a version bump
// cannot break it: a read whose single return is a bool, a number or a string,
// which is enough to say the id resolved, the member was found on the object,
// and the value came back through the wire at the offset the table claims.
func TestEveryClassDispatchesAtLeastOneMember(t *testing.T) {
	a := loadTestAPI(t)
	r := GenerateMembers(a)
	src, err := r.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	idx := r.MemberIndex()

	// probe is one class's chosen member and what to expect back.
	type probe struct {
		class, name string
		id          int
		// reader names how the Lua side reads the value back. A NAME rather
		// than the Kind's number: the numbers are an ABI surface shared with
		// fk_abi.lua, and copying them into a test is how a test starts
		// checking the wrong offset without saying so.
		reader    string
		lua, want string // the stub's value, and the rendering to compare
	}
	var probes []probe
	seen := map[string]bool{}

	// Deterministic order: r.Members is already stable, and taking the FIRST
	// usable member of each class means the choice does not drift between runs.
	for _, m := range r.Members {
		if m.Class == "" || seen[m.Class] || m.Kind != MemberGet {
			continue
		}
		_, rets, err := m.blocks()
		if err != nil || len(rets.Fields) != 1 {
			continue
		}
		f := rets.Fields[0]
		if f.HasOffset >= 0 || f.Offset != 0 {
			continue // an optional or a padded return needs its own reader
		}
		id, ok := idx[fmt.Sprintf("%s::%s/%d", m.Class, m.Name, m.Kind)]
		if !ok {
			continue
		}
		p := probe{class: m.Class, name: m.Name, id: id}
		switch f.Kind {
		case KindBool:
			p.reader, p.lua, p.want = "bool", "true", "true"
		case KindF64:
			p.reader, p.lua, p.want = "f64", "1.5", "1.5"
		case KindU32:
			p.reader, p.lua, p.want = "u32", "7", "7"
		case KindString:
			p.reader, p.lua, p.want = "str", `"ok"`, "ok"
		default:
			continue
		}
		seen[m.Class] = true
		probes = append(probes, p)
	}

	// Which classes got none? Reported rather than ignored: a class with no
	// simply-shaped read is a real hole in this gate, and the list is what says
	// whether the hole is two classes or fifty.
	var uncovered []string
	for _, c := range a.Classes {
		if !seen[c.Name] {
			uncovered = append(uncovered, c.Name)
		}
	}
	sort.Strings(uncovered)
	t.Logf("%d of %d classes have a simply-shaped attribute read", len(probes), len(a.Classes))
	if len(uncovered) > 0 {
		t.Logf("not covered by this gate (%d): %s", len(uncovered), strings.Join(uncovered, " "))
	}

	// One Lua table of probes, one loop. Emitting 156 inline call sites would
	// blow past Lua's 200-local limit in the driver chunk itself.
	var b strings.Builder
	// A string coming back is written through the guest's allocator, and
	// without one bound the host returns ERR_BAD_ARGS rather than guessing --
	// which is right, and which is also why 72 of these read as failures until
	// the harness grew a bump allocator. Nothing here frees; the test is short.
	b.WriteString(`local API = require("fk_api_gen")
H.bind_members(API.members)
local BUMP = 16384
H.bind_alloc(function(n) local p = BUMP BUMP = BUMP + n + 8 return p end,
             function(_) end)
local P = {
`)
	for _, p := range probes {
		fmt.Fprintf(&b, "  { %d, %q, %s, %q, %q },\n", p.id, p.name, p.lua, p.want, p.reader)
	}
	b.WriteString(`}
local bad = 0
for _, e in ipairs(P) do
  local id, name, value, want, reader = e[1], e[2], e[3], e[4], e[5]
  local stub = { valid = true }
  stub[name] = value
  local st = H.call(H.transient(stub), id, 0, 4096)
  local got
  if st ~= 0 then
    got = "status " .. st
  elseif reader == "bool" then
    got = tostring(IO.ld8(4096) ~= 0)
  elseif reader == "str" then
    got = M.read_string(IO.ld32(4096), IO.ld32(4100))
  elseif reader == "f64" then
    got = tostring(IO.ldf64(4096))
  else
    got = tostring(IO.ld32(4096))
  end
  if got ~= want then
    bad = bad + 1
    print("MISMATCH " .. name .. ": got " .. got .. ", want " .. want)
  end
end
print("checked " .. #P .. ", mismatched " .. bad)
`)

	got := runMarshalWithFile(t, "fk_api_gen.lua", src, b.String())
	want := fmt.Sprintf("checked %d, mismatched 0", len(probes))
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
