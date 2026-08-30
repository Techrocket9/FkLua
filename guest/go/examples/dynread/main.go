// Command dynread is the end-to-end fixture for READING a tier-2 value.
//
// Tier 2 had seven constructors and no accessors, so every guest that received
// one wrote its own linear scan and tag switch -- and the scans in this repo's
// own examples read kv.Val.Str without ever looking at kv.Val.Tag, which is the
// empty string for a number and for an absent key alike. This is that surface
// exercised against a value the HOST built, which is the only place the
// accessors can be wrong in a way a type checker cannot see: a lookup that
// matched a key of the wrong tag, a chained miss that returned something other
// than nil, an At that walked a map's pair slice as if it were an array.
//
// json_to_table is the source because it is one member, its return is a bare
// tier-2 value, and the stub that stands in for it needs nothing but a table
// literal -- so what this fixture asserts is the accessors and not the
// plumbing around them.
//
// EVERY LINE HERE IS ORDER-INDEPENDENT, deliberately. The host writes a map's
// pairs in pairs() order, which this ABI does not promise and which bin/lua52f
// varies between runs, so a fixture that printed the pair slice would be
// asserting on something nobody owes it. Key lookups and Len are the whole
// surface below, and At runs over an ARRAY, which is index-ordered.
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func ok(b bool) string {
	if b {
		return "ok"
	}
	return "no"
}

//go:wasmexport fk_on_init
func onInit() {
	v, err := fkapi.Helpers.JsonToTable("{}")
	if err != nil {
		fk.Log("dynread: " + err.Error())
		return
	}
	if v == nil {
		fk.Log("dynread: absent")
		return
	}
	t := *v

	// A LOOKUP CHAINS BECAUSE ITS MISS IS NIL. Every line here is one
	// expression over a shape that may not be there, which is the whole reason
	// Get returns a Value rather than a comma-ok.
	fk.Log("get: name=" + t.Get("name").StrOr("?") +
		" count=" + num(t.Get("count").NumOr(-1)) +
		" on=" + strconv.FormatBool(t.Get("on").BoolOr(false)))
	fk.Log("miss: str=" + t.Get("nope").StrOr("<none>") +
		" num=" + num(t.Get("nope").NumOr(-1)) +
		" nil=" + strconv.FormatBool(t.Get("nope").IsNil()))

	// HAS IS THE QUESTION GET CANNOT ANSWER. A key present and nil reads
	// exactly like a key that is absent -- which is unreachable through a Lua
	// table literal, because assigning nil is how you remove a key, so what
	// this pins is the pair of answers a real option table can produce.
	fk.Log("has: name=" + strconv.FormatBool(t.Has("name")) +
		" nope=" + strconv.FormatBool(t.Has("nope")) +
		" onascalar=" + strconv.FormatBool(t.Get("count").Has("name")))

	// CHAINED THROUGH TWO LEVELS, and through a level that is not there.
	fk.Log("deep: hit=" + num(t.Get("inner").Get("deep").NumOr(-1)) +
		" miss=" + num(t.Get("inner").Get("gone").NumOr(-1)) +
		" via-scalar=" + num(t.Get("count").Get("deep").NumOr(-1)))

	// AN ARRAY, ZERO-BASED -- the one-based Lua index the host read it out of
	// is behind us. Out of range and wrong-container both answer nil.
	//
	// THE MAP CASE IS ASSERTED THROUGH IsNil AND NOT ONLY THROUGH StrOr, and
	// that is a red proof's doing rather than belt and braces: an At that walked
	// a map's PAIR SLICE as if it were an array would hand back whichever pair
	// came first in pairs() order, and a default of "<notarray>" hides that
	// whenever the first pair's value is not a string -- which is most of the
	// time, and is nondeterministic. IsNil answers the same question for every
	// tag, so the line fails every run instead of some of them.
	a := t.Get("list")
	fk.Log("at: 0=" + a.At(0).StrOr("?") + " 2=" + a.At(2).StrOr("?") +
		" 9=" + a.At(9).StrOr("<oob>") + " neg=" + a.At(-1).StrOr("<oob>") +
		" map=" + t.At(0).StrOr("<notarray>") +
		" map-nil=" + strconv.FormatBool(t.At(0).IsNil()))

	// LEN IS THE ONE ACCESSOR WITH AN ANSWER FOR A SCALAR, and the answer is
	// none rather than a refusal.
	fk.Log("len: map=" + strconv.Itoa(t.Len()) + " arr=" + strconv.Itoa(a.Len()) +
		" scalar=" + strconv.Itoa(t.Get("count").Len()) +
		" nil=" + strconv.Itoa(t.Get("nope").Len()))

	// NOTHING COERCES. Each of these is the payload read through the WRONG
	// tag, and every one of them is not-ok rather than a plausible zero.
	n, okn := t.Get("name").AsNum()
	s, oks := t.Get("count").AsStr()
	b, okb := t.Get("name").AsBool()
	fk.Log("as: num-of-str=" + num(n) + "/" + ok(okn) +
		" str-of-num='" + s + "'/" + ok(oks) +
		" bool-of-str=" + strconv.FormatBool(b) + "/" + ok(okb))

	// ...and each read through the RIGHT tag, which is the control: a family
	// that answered no to everything would pass every line above.
	n2, okn2 := t.Get("count").AsNum()
	s2, oks2 := t.Get("name").AsStr()
	b2, okb2 := t.Get("on").AsBool()
	fk.Log("as: num=" + num(n2) + "/" + ok(okn2) +
		" str='" + s2 + "'/" + ok(oks2) +
		" bool=" + strconv.FormatBool(b2) + "/" + ok(okb2))

	// A NUMBER KEY, which Get cannot spell. Equality is by tag and payload, so
	// the number 7 and the string "7" are different keys.
	fk.Log("key: n7=" + t.GetKey(fkapi.OfNumber(7)).StrOr("?") +
		" s7=" + t.GetKey(fkapi.OfString("7")).StrOr("<none>") +
		" n8=" + t.GetKey(fkapi.OfNumber(8)).StrOr("<none>"))

	// A HANDLE THROUGH A TIER-2 VALUE, and it still resolves -- which is what
	// says AsObj hands back the object rather than a number that looks like
	// one.
	h, okh := t.Get("obj").AsObj()
	if !okh {
		fk.Log("obj: not an object")
		return
	}
	name, err := fkapi.LuaEntity{Object: h}.Name()
	if err != nil {
		fk.Log("obj: " + err.Error())
		return
	}
	fk.Log("obj: " + name + " zero=" + strconv.FormatUint(uint64(t.Get("nope").ObjOr(fkapi.Object{}).Handle()), 10))
}

func main() {}
