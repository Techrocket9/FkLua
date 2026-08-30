package factorio

import "testing"

// THE ABI SIGNATURE, and the property the whole mechanism rests on: it moves
// when the packaged table's MEANING moves, and not otherwise.
//
// The pin stamp proves a guest and a table came from one DESCRIPTION. It cannot
// prove they came from one GENERATION, and at one pin the ids move whenever the
// generator grows -- so a wasm built against older bindings and packaged with a
// fresh member table at the same pin passes every check there is, with every id
// resolving to a different member. See pin.go, and BetterBeltBalancer's
// FKLUA-GAPS item 18.

// A LAYOUT CHANGE WITH NO ID MOVEMENT MOVES THE DIGEST, which is exactly the
// class an id-only digest would be blind to and the class this round produced:
// LuaGuiElement::style's write half went from an Object setter to a Value one,
// same member, same id, different wire. A guest compiled against the old
// bindings encodes a handle where the table now says tier 2.
//
// Asserted by EDITING A LOADED DESCRIPTION rather than by comparing two
// committed ones, because two versions differ in a hundred ways at once and this
// has to isolate the one.
func TestTheSignatureSeesALayoutChangeWithNoIdMovement(t *testing.T) {
	a := loadTestAPI(t)
	before := APISignature(a)

	var found bool
	for i := range a.Classes {
		if a.Classes[i].Name != "LuaGuiElement" {
			continue
		}
		for j := range a.Classes[i].Attributes {
			if a.Classes[i].Attributes[j].Name == "style" {
				a.Classes[i].Attributes[j].WriteType = &Type{Name: "string"}
				found = true
			}
		}
	}
	if !found {
		t.Skip("this description has no LuaGuiElement::style to retype")
	}

	after := APISignature(a)
	if after == before {
		t.Errorf("the digest is blind to a member's LAYOUT: %s either side of "+
			"style's write half changing from a handle to a string. An id-only "+
			"digest would miss every generator change that retypes a member "+
			"without moving one", before)
	}
	// ...and the ids really did not move, which is what makes this the case the
	// pin stamp cannot see.
	if n1, n2 := len(GenerateMembers(loadTestAPI(t)).Members),
		len(GenerateMembers(a).Members); n1 != n2 {
		t.Errorf("the edit moved the member COUNT (%d then %d), so this test is "+
			"not about a layout change at all", n1, n2)
	}
}

// ...AND A MEMBER APPEARING MOVES IT TOO, which is the ordinary case.
func TestTheSignatureSeesAMemberAppearing(t *testing.T) {
	a := loadTestAPI(t)
	before := APISignature(a)
	for i := range a.Classes {
		if a.Classes[i].Name != "LuaGuiElement" {
			continue
		}
		a.Classes[i].Attributes = append(a.Classes[i].Attributes, Attribute{
			Name: "aaa_fklua_probe", ReadType: &Type{Name: "boolean"},
		})
	}
	if APISignature(a) == before {
		t.Errorf("the digest did not move when a member was added: %s", before)
	}
}
