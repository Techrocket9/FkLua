package factorio

import (
	"strings"
	"testing"
)

// AN OPTIONAL ATTRIBUTE THAT IS ABSENT, AND THE ONE STATUS IT MUST NOT BE.
//
// `runtime-api.json` declares a large minority of its readable attributes
// `optional: true` -- `optional_readable_attributes` in census.json is the
// live count, and `LuaEntity.temperature` is one of them, present on a reactor
// and absent on a chest. The generator honoured `optional` on METHOD returns
// and dropped it on attributes, so every one of them was typed as
// always-present on both sides and an absent one came back as **ERR_NO_MEMBER,
// "no such member on this Factorio version"** -- which is not what happened.
// The member exists; the value is nil.
//
// It is the same status a member genuinely REMOVED in a Factorio point release
// produces, so a guest could not distinguish "this object has no temperature"
// from "this Factorio does not have this attribute" -- the one distinction
// ERR_NO_MEMBER exists to make. Found downstream (fklua-ports-samples, Q4):
//
//	probe temperature logistic-chest err=3 no such member on this Factorio version
//	[IS] probe temperature storage-tank   err=3 no such member on this Factorio version
//
// ...against a nuclear reactor in the same save answering Ok(15.0).
//
// TWO THINGS WERE MISSING AND EITHER ALONE LEAVES THE DEFECT. The member table
// had no `has=` for the return, so `encode_rets` had no presence byte to write;
// and `M.invoke` turns a nil value into ERR_NO_MEMBER before `encode_rets` ever
// runs, so the layout on its own changes nothing. The fix is `opt=true` on the
// member entry -- the generator's statement that nil is a legal value here --
// which is what keeps the distinction rather than erasing it: nil still means
// ERR_NO_MEMBER everywhere the description did NOT say optional.

// A stand-in object with one attribute present and one absent, plus the member
// table to read them through. Reused by every case below.
const optAttrSetup = `
local ent = { valid = true, temperature = 15.0, colour = nil, name = "reactor" }
H.bind_globals({})
H.bind_members({
  -- 1 and 2 are the SAME shape and differ only in opt=: what the generator
  -- knew about the description.
  [1] = { kind = H.GET, name = "temperature", class = "LuaEntity", valid = true,
          opt = true, argsize = 0, retsize = 16,
          sig = { args = {}, rets = { { name = "value", kind = H.K_F64, at = 8, has = 0 } } } },
  [2] = { kind = H.GET, name = "colour", class = "LuaEntity", valid = true,
          opt = true, argsize = 0, retsize = 16,
          sig = { args = {}, rets = { { name = "value", kind = H.K_F64, at = 8, has = 0 } } } },
  -- ...and 3 is the control: not declared optional, so nil keeps meaning
  -- "this Factorio does not have it".
  [3] = { kind = H.GET, name = "colour", class = "LuaEntity", valid = true,
          argsize = 0, retsize = 8,
          sig = { args = {}, rets = { { name = "value", kind = H.K_F64, at = 0 } } } },
}
)
local h = H.transient(ent)
`

// The whole finding, at the smallest scale that can express it.
func TestAnAbsentOptionalAttributeIsAbsentRatherThanMissing(t *testing.T) {
	got := runMarshal(t, optAttrSetup+`
-- Present: the presence byte is set and the value is there.
IO.st8(0, 0)
print("present " .. H.call(h, 1, 0, 0) .. " has=" .. IO.ld8(0) .. " v=" .. IO.ldf64(8))

-- Absent: OK, presence byte clear, and the guest reads nothing out of the slot
-- it already zeroed.
IO.st8(32, 1) IO.stf64(40, 99)
print("absent " .. H.call(h, 2, 0, 32) .. " has=" .. IO.ld8(32))

-- And the control keeps the old meaning, so the distinction is real rather than
-- traded away: an attribute the description did NOT call optional is still
-- ERR_NO_MEMBER when it reads nil.
print("control " .. H.call(h, 3, 0, 64))
`)
	want := strings.Join([]string{
		"present 0 has=1 v=15",
		"absent 0 has=0",
		"control " + itoaTest(3), // ERR_NO_MEMBER
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(an absent optional attribute must be ABSENT, "+
			"not ERR_NO_MEMBER -- a guest cannot tell that apart from an attribute "+
			"this Factorio version removed, which is the one thing the status is for)",
			got, want)
	}
}

// The host-side string predicate over an optional attribute.
//
// `LuaEntity.backer_name` and 29 others are optional plain strings, and the EQ
// kind is offered for them: nil is not the string, so the honest answer is
// false, and `call_eq`'s `type(f) == "string"` already produced it. What did NOT
// work was reaching `call_eq` at all -- `M.invoke`'s nil check fired first and
// the guest got ERR_NO_MEMBER for a chest with no backer name.
func TestAnAbsentOptionalStringComparesFalseRatherThanFailing(t *testing.T) {
	got := runMarshal(t, `
local ent = { valid = true, backer_name = nil, name = "iron-chest" }
H.bind_globals({})
H.bind_members({
  [1] = { kind = H.EQ, name = "backer_name", class = "LuaEntity", valid = true,
          opt = true, argsize = 8, retsize = 1,
          sig = { args = { { name = "want", kind = H.K_STR, at = 0 } },
                  rets = { { name = "value", kind = H.K_BOOL, at = 0 } } } },
  [2] = { kind = H.EQ, name = "name", class = "LuaEntity", valid = true,
          argsize = 8, retsize = 1,
          sig = { args = { { name = "want", kind = H.K_STR, at = 0 } },
                  rets = { { name = "value", kind = H.K_BOOL, at = 0 } } } },
})
local h = H.transient(ent)
-- "iron-chest" at byte 64.
local s = "iron-chest"
for i = 1, #s do IO.st8(63 + i, string.byte(s, i)) end
IO.st32(0, 64) IO.st32(4, #s)

print("absent " .. H.call(h, 1, 0, 16) .. " eq=" .. IO.ld8(16))
print("present " .. H.call(h, 2, 0, 17) .. " eq=" .. IO.ld8(17))
`)
	want := "absent 0 eq=0\npresent 0 eq=1"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(an absent optional string is not equal to "+
			"the string asked about; that is a false, not a failure)", got, want)
	}
}

// A SET is exempt and stays exempt, for the reason M.invoke already gives:
// assigning a field that reads as nil is how you create it. `opt` must not
// change that.
func TestAnOptionalAttributeCanStillBeSetWhenAbsent(t *testing.T) {
	got := runMarshal(t, `
local ent = { valid = true }
H.bind_globals({})
H.bind_members({
  [1] = { kind = H.SET, name = "colour", class = "LuaEntity", valid = true,
          opt = true, argsize = 8, retsize = 0,
          sig = { args = { { name = "value", kind = H.K_F64, at = 0 } }, rets = {} } },
})
local h = H.transient(ent)
IO.stf64(0, 42)
print("set " .. H.call(h, 1, 0, 32) .. " v=" .. tostring(ent.colour))
`)
	if want := "set 0 v=42"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// itoaTest keeps the expected status readable without importing strconv for one
// number; ERR_NO_MEMBER is 3 and saying so twice is how a constant drifts.
func itoaTest(n int) string { return string(rune('0' + n)) }
