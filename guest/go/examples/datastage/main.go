// Command datastage is the data-stage end-to-end guest: a Go program that
// becomes a Factorio mod's settings.lua, data.lua and data-final-fixes.lua.
//
// It is the fixture the data-stage tests build and the in-game `--dump-data`
// gate packages, so it is written to reach every one of the seven imports
// rather than to be a hello-world: it declares a setting, defines prototypes
// from a computed loop, reads a value back out of data.raw and extends
// something derived from it, deep-copies a base prototype and patches the copy,
// enumerates a prototype type in sorted order, and asks whether a prototype it
// might collide with is already defined.
//
// IT IS ALSO THE RUST MIRROR'S TWIN. guest/rust/examples/datastage is a
// line-for-line port, and one test drives both through one fk_data.lua and
// requires the same call sequence out of each, which is the only thing that
// stops the two guest libraries drifting.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -opt=2 \
//	    -o datastage.wasm ./examples/datastage
//	fklua mod control.wasm --data-module datastage.wasm --name fk-datastage ...
package main

import (
	"strconv"

	"github.com/Techrocket9/fklua/guest/go/fkdata"
)

// The mod's own prefix, so nothing here can collide with a real mod.
const prefix = "fkd-"

//go:wasmexport fk_settings
func onSettings() {
	fkdata.Log("fkdata example: settings stage")

	// A startup setting. At this stage `settings` itself does not exist yet --
	// a mod's settings are not readable while they are being declared -- so
	// asking is the way to show that, rather than a comment.
	if _, ok := fkdata.StartupSetting(prefix + "enabled"); ok {
		fkdata.Log("fkdata example: a startup setting is readable at the settings stage")
	}

	fkdata.Extend(fkdata.Obj(
		fkdata.KVs("type", fkdata.Str("bool-setting")),
		fkdata.KVs("name", fkdata.Str(prefix+"enabled")),
		fkdata.KVs("setting_type", fkdata.Str("startup")),
		fkdata.KVs("default_value", fkdata.Bool(true)),
		fkdata.KVs("order", fkdata.Str("a")),
	))
}

//go:wasmexport fk_data
func onData() {
	fkdata.Log("fkdata example: data stage, base " + baseVersion())

	// The mod's own name, from the packager through env(4): the prefix a
	// library would derive instead of hardcoding, logged so the in-game run
	// and the mirror test both pin that it arrives.
	fkdata.Log("fkdata example: mod name is " + fkdata.ModName())

	// defines.prototypes through env(5): the base-type map a prototype browser
	// needs and data.raw alone cannot answer. One line, both accessors.
	base, _ := fkdata.BaseType("transport-belt")
	fkdata.Log("fkdata example: transport-belt is an " + base + "; item derives " +
		strconv.Itoa(len(fkdata.DerivedTypes("item"))) + " types")

	// THE SETTINGS -> DATA ROUND TRIP. The setting fk_settings declared comes
	// back here through env(3), and its value lands in the DUMP -- the marker
	// technology's enabled field below -- so the in-game gate proves the loop
	// end to end instead of assuming it. Logged too, because a dump hash that
	// matched while the read silently answered absent is the vacuous pass this
	// has to be able to fail on.
	enabled, haveSetting := fkdata.StartupSetting(prefix + "enabled")
	if haveSetting {
		fkdata.Log("fkdata example: startup " + prefix + "enabled is " +
			strconv.FormatBool(enabled.Boolean()))
	} else {
		fkdata.Log("fkdata example: startup " + prefix + "enabled is ABSENT")
	}

	// A computed table. Eight sprites out of one loop, with the offset
	// arithmetic done in Go rather than written out as sixteen magic numbers --
	// which is the case the whole feature exists for.
	//
	// A `var` RATHER THAN A `const`, AND THE MIRROR TEST IS WHY. Go's untyped
	// constants are arbitrary-precision, so `0.3 + bias` with a const bias folds
	// exactly and then rounds ONCE, giving 0.40400000000000003; Rust's
	// `const BIAS: f64` folds under IEEE f64 rules and gives 0.40399999999999997.
	// Both are defensible and they are different doubles, so a const here would
	// make the two example guests emit different prototypes -- which
	// TestBothDataGuestLibrariesMakeTheSameCalls found on its first run, and
	// which is a fact about the two LANGUAGES rather than about the two data
	// libraries. A `var` forces f64 arithmetic in both.
	//
	// It does not reach the game: a sprite's shift is a float32 in the
	// prototype, so both doubles narrow to the same f32 and --dump-data hashes
	// identically either way. It would reach the game for any field the engine
	// keeps as a double.
	bias := 0.104
	sides := []struct {
		n    string
		x, y float64
	}{{"n", 0, -1}, {"e", 1, 0}, {"s", 0, 1}, {"w", -1, 0}}

	var sprites []fkdata.V
	for kind, dir := range []string{"in", "out"} {
		d := 0.3 - bias
		if dir == "out" {
			d = 0.3 + bias
		}
		for i, s := range sides {
			sprites = append(sprites, fkdata.Obj(
				fkdata.KVs("type", fkdata.Str("sprite")),
				fkdata.KVs("name", fkdata.Str(prefix+"arrow-"+dir+"-"+s.n)),
				fkdata.KVs("filename", fkdata.Str("__core__/graphics/empty.png")),
				fkdata.KVs("width", fkdata.Num(1)),
				fkdata.KVs("height", fkdata.Num(1)),
				fkdata.KVs("x", fkdata.Num(float64((kind*4+i)*32))),
				fkdata.KVs("scale", fkdata.Num(0.5)),
				fkdata.KVs("shift", fkdata.Arr(fkdata.Num(s.x*d), fkdata.Num(s.y*d))),
				fkdata.KVs("flags", fkdata.Arr(fkdata.Str("no-crop"))),
			))
		}
	}
	fkdata.Extend(sprites...)

	// Read then extend: a technology whose research cost is base's own, rather
	// than a copy of it that goes stale when base moves.
	count, haveCount := fkdata.Get("technology", "logistics", "unit", "count")
	time, _ := fkdata.Get("technology", "logistics", "unit", "time")
	ingredients, _ := fkdata.Get("technology", "logistics", "unit", "ingredients")
	if haveCount {
		fkdata.Extend(fkdata.Obj(
			fkdata.KVs("type", fkdata.Str("technology")),
			fkdata.KVs("name", fkdata.Str(prefix+"marker")),
			fkdata.KVs("icon", fkdata.Str("__core__/graphics/empty.png")),
			fkdata.KVs("icon_size", fkdata.Num(1)),
			fkdata.KVs("effects", fkdata.Arr()),
			// The startup setting's value, dump-visible: the round trip's
			// in-game half.
			fkdata.KVs("enabled", fkdata.Bool(enabled.Boolean())),
			fkdata.KVs("prerequisites", fkdata.Arr(fkdata.Str("logistics"))),
			fkdata.KVs("unit", fkdata.Obj(
				fkdata.KVs("count", count),
				fkdata.KVs("time", time),
				fkdata.KVs("ingredients", ingredients),
			)),
			fkdata.KVs("order", fkdata.Str("a-"+prefix)),
		))
	}

	// Clone and patch: the shape a hidden prototype takes, and the one thing
	// that cannot be done by reading a prototype into the guest and writing it
	// back without risking every leaf it does not touch.
	fkdata.Clone("transport-belt", "transport-belt", prefix+"belt")
	fkdata.Set(fkdata.Num(0.25), "transport-belt", prefix+"belt", "speed")
	fkdata.Set(fkdata.Nil(), "transport-belt", prefix+"belt", "minable")
	fkdata.Set(fkdata.Nil(), "transport-belt", prefix+"belt", "next_upgrade")
	fkdata.Set(fkdata.Arr(fkdata.Str("not-on-map")), "transport-belt", prefix+"belt", "flags")

	// A NESTED patch, through NUMERIC path elements -- collision_box is an array
	// of two arrays, so this reaches four leaves two levels down.
	//
	// IT IS WHAT TELLS A DEEP COPY FROM A SHALLOW ONE, which is why it is here
	// rather than in a test: under a shallow clone `collision_box` is the SOURCE's
	// table, so these four writes would silently shrink base's own transport belt
	// and the acceptance dump would say so. Every patch above is top-level and a
	// shallow clone survives all of them.
	for _, at := range [][3]float64{{1, 1, -0.35}, {1, 2, -0.35}, {2, 1, 0.35}, {2, 2, 0.35}} {
		fkdata.Set(fkdata.Num(at[2]), "transport-belt", prefix+"belt", "collision_box",
			int(at[0]), int(at[1]))
	}

	// The sorted enumeration primitive, and what it is FOR: the fastest belt in
	// the game, with ties broken by a sorted name rather than by whichever
	// order the mods happened to load in.
	best, bestName := -1.0, ""
	for _, name := range fkdata.Keys("transport-belt") {
		if s, ok := fkdata.Get("transport-belt", name, "speed"); ok && s.Number() > best {
			best, bestName = s.Number(), name
		}
	}
	fkdata.Log("fkdata example: fastest belt is " + bestName)
}

//go:wasmexport fk_data_final_fixes
func onFinalFixes() {
	fkdata.Log("fkdata example: " + fkdata.Stage().Name() + " stage")

	// The absence question, which is the one case the ABI answers with a status
	// rather than a raise: has anybody else already defined this?
	if _, taken := fkdata.Get("item", prefix+"token"); !taken {
		fkdata.Extend(fkdata.Obj(
			fkdata.KVs("type", fkdata.Str("item")),
			fkdata.KVs("name", fkdata.Str(prefix+"token")),
			fkdata.KVs("icon", fkdata.Str("__core__/graphics/empty.png")),
			fkdata.KVs("icon_size", fkdata.Num(1)),
			fkdata.KVs("stack_size", fkdata.Num(42)),
			fkdata.KVs("flags", fkdata.Arr()),
		))
	}
}

// baseVersion is the version branch every real data stage has, and the reason a
// data guest and a control guest want to be two main packages in ONE Go module:
// this function is ordinary Go, so both can import it from a shared package
// instead of the two Lua stages each parsing their own copy.
func baseVersion() string {
	v, ok := fkdata.ModVersion("base")
	if !ok {
		return "absent"
	}
	return v
}
