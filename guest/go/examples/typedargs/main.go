// Command typedargs is the end-to-end fixture for the TYPED ARGUMENT form of a
// method whose parameter table is a discriminated union.
//
// LuaGuiElement::add is the member the survey is about: 22 variant groups and
// 341 possible keys defeat the typed-args generator, so every element was a
// hand-built nested pair list, and a tier-2 map is measured at 3.3x the host
// cost of a flat block. AddTyped is the same member id over a tier-1 struct plus
// one optional tier-2 slot for the variant tail.
//
// WHAT THIS ASSERTS IS AN EQUALITY, and that is the whole design of the fixture:
// the same spec is built BOTH ways and passed through both imports, and the
// stub renders the Lua table it was finally handed. If the two renderings differ
// the typed decode landed somewhere the dyn decode did not, which is the only
// way this feature can be wrong without a type checker seeing it. The stub sorts
// its keys, so the rendering is a function of the table's contents and not of
// pairs() order.
//
// Four legs, in order: the two encodings of one spec; the tail applied over the
// block, which is the escape hatch doing its job; the tail ABSENT, which must
// not leave a stale key behind; and the tail OVERRIDING a key the block already
// set, which is the rule create_entity's `target` makes necessary.
package main

import (
	"github.com/Techrocket9/fklua/guest/go/fk"
	"github.com/Techrocket9/fklua/guest/go/fkapi"
)

func str(s string) *string { return &s }

func val(v fkapi.Value) *fkapi.Value { return &v }

//go:wasmexport fk_on_init
func onInit() {
	guiLegs()
	entityLegs()
}

// THE GUI LEGS, which are the equality this fixture is for and which no
// HEADLESS run can reach: LuaGuiElement::add needs a player, and a headless
// --create has none. So in game this logs one line and stops, and under the
// host stub it runs -- which is why the entity legs below exist beside it.
func guiLegs() {
	p, err := fkapi.Game.GetPlayer(fkapi.OfNumber(1))
	if err != nil || p == nil {
		fk.Log("gui: no player")
		return
	}
	g, err := fkapi.LuaPlayer{Object: *p}.Gui()
	if err != nil {
		fk.Log("typedargs: " + err.Error())
		return
	}
	sc, err := fkapi.LuaGui{Object: g}.Screen()
	if err != nil {
		fk.Log("typedargs: " + err.Error())
		return
	}
	screen := fkapi.LuaGuiElement{Object: sc}

	// LEG 1 -- THE DYN FORM, which is what a guest writes today: a pair list of
	// key strings, with nothing checking that "caption" is spelled right.
	fk.Log("leg dyn")
	_, err = screen.Add(fkapi.OfMap(
		fkapi.KeyValue{Key: fkapi.OfString("type"), Val: fkapi.OfString("button")},
		fkapi.KeyValue{Key: fkapi.OfString("name"), Val: fkapi.OfString("row-7")},
		fkapi.KeyValue{Key: fkapi.OfString("caption"), Val: fkapi.OfString("Launch")},
		fkapi.KeyValue{Key: fkapi.OfString("style"), Val: fkapi.OfString("green_button")},
		fkapi.KeyValue{Key: fkapi.OfString("enabled"), Val: fkapi.OfBool(true)},
	))
	if err != nil {
		fk.Log("typedargs: dyn add: " + err.Error())
		return
	}

	// LEG 2 -- THE SAME SPEC, TYPED. The stub must render this identically.
	fk.Log("leg typed")
	yes := true
	_, err = screen.AddTyped(fkapi.LuaGuiElementAddArgs{
		Type:    "button",
		Name:    str("row-7"),
		Caption: val(fkapi.OfString("Launch")),
		Style:   str("green_button"),
		Enabled: &yes,
	}, nil)
	if err != nil {
		fk.Log("typedargs: typed add: " + err.Error())
		return
	}

	// LEG 3 -- THE VARIANT TAIL, which is what the block cannot express: a
	// button's `sprite` is in a variant group, so it has no field.
	fk.Log("leg tail")
	_, err = screen.AddTyped(fkapi.LuaGuiElementAddArgs{
		Type: "sprite-button",
		Name: str("icon"),
	}, val(fkapi.OfMap(
		fkapi.KeyValue{Key: fkapi.OfString("sprite"), Val: fkapi.OfString("item/iron-plate")},
		fkapi.KeyValue{Key: fkapi.OfString("number"), Val: fkapi.OfNumber(42)},
	)))
	if err != nil {
		fk.Log("typedargs: tail add: " + err.Error())
		return
	}

	// LEG 4 -- THE TAIL OVERRIDES THE BLOCK. A shared parameter and a
	// variant-group parameter may share a NAME (create_entity's `target` does at
	// every committed pin), so the tail has to win or it is not an escape hatch.
	fk.Log("leg override")
	_, err = screen.AddTyped(fkapi.LuaGuiElementAddArgs{
		Type: "label",
		Name: str("block-said-this"),
	}, val(fkapi.OfMap(
		fkapi.KeyValue{Key: fkapi.OfString("name"), Val: fkapi.OfString("tail-said-this")},
	)))
	if err != nil {
		fk.Log("typedargs: override add: " + err.Error())
		return
	}

	// LEG 5 -- AN ABSENT OPTIONAL IS ABSENT, not present-and-zero. The block is
	// 248 bytes of mostly-optional fields and the presence bytes are the only
	// thing between "say nothing" and "say no", which is the F2 shape.
	fk.Log("leg minimal")
	_, err = screen.AddTyped(fkapi.LuaGuiElementAddArgs{Type: "flow"}, nil)
	if err != nil {
		fk.Log("typedargs: minimal add: " + err.Error())
		return
	}
	fk.Log("gui done")
}

// THE ENTITY LEGS, which a headless run CAN reach: a surface exists at tick 0
// and create_entity is the other variant-defeated member. This is the in-game
// proof that a typed block reaches the real engine -- an entity the game
// actually built, read back by name -- and it runs under the host stub too, so
// the fixture asserts one transcript for both worlds.
func entityLegs() {
	s, err := fkapi.Game.GetSurface(fkapi.OfNumber(1))
	if err != nil || s == nil {
		fk.Log("entity: no surface")
		return
	}
	surf := fkapi.LuaSurface{Object: *s}

	// The dyn form, at one position.
	e1, err := surf.CreateEntity(fkapi.OfMap(
		fkapi.KeyValue{Key: fkapi.OfString("name"), Val: fkapi.OfString("iron-chest")},
		fkapi.KeyValue{Key: fkapi.OfString("position"), Val: fkapi.OfArray(
			fkapi.OfNumber(8), fkapi.OfNumber(8))},
	))
	if err != nil {
		fk.Log("entity: dyn create: " + err.Error())
		return
	}
	fk.Log("entity dyn: " + nameOf(e1))

	// ...and the TYPED form at another, which is the whole in-game claim: a
	// tier-1 block decoded by M.call_typed reaching the same engine call.
	e2, err := surf.CreateEntityTyped(fkapi.LuaSurfaceCreateEntityArgs{
		Name:     fkapi.OfString("iron-chest"),
		Position: fkapi.MapPosition{X: 12, Y: 8},
	}, nil)
	if err != nil {
		fk.Log("entity: typed create: " + err.Error())
		return
	}
	fk.Log("entity typed: " + nameOf(e2))
	fk.Log("done")
}

// nameOf reads the entity back through the API rather than trusting the handle,
// so what the line reports is a thing the ENGINE built and not a number the
// binding returned.
func nameOf(o *fkapi.Object) string {
	if o == nil {
		return "<none>"
	}
	n, err := fkapi.LuaEntity{Object: *o}.Name()
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return n
}

// ONE MORE TYPED CALL, ON A TICK, so a headless run has something to compare
// between its two replays. fk_on_init runs while the map is being CREATED and
// its output is in the create log, not the benchmark's -- so without this the
// determinism check would have no line of ours to count.
//
// EXPORTING fk_on_tick IS THE SUBSCRIPTION, and the tick number is what gates
// the work: creating an entity every tick would pile them on one tile and the
// second attempt would fail on a collision rather than on anything this proves.
//
//go:wasmexport fk_on_tick
func onTick() {
	t, err := fkapi.Game.Tick()
	if err != nil || t != 1 {
		return
	}
	s, err := fkapi.Game.GetSurface(fkapi.OfNumber(1))
	if err != nil || s == nil {
		return
	}
	e, err := fkapi.LuaSurface{Object: *s}.CreateEntityTyped(fkapi.LuaSurfaceCreateEntityArgs{
		Name:     fkapi.OfString("iron-chest"),
		Position: fkapi.MapPosition{X: 16, Y: 8},
	}, nil)
	if err != nil {
		fk.Log("tick typed: " + err.Error())
		return
	}
	fk.Log("tick typed: " + nameOf(e))
}

func main() {}
