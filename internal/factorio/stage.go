package factorio

import (
	"fmt"
	"sort"
	"strings"
)

// The DATA STAGE, from a guest.
//
// Factorio loads a mod in two halves. The CONTROL stage is a running program
// with events, ticks and saved state, and it is what `fklua mod` has always
// compiled. The SETTINGS and DATA stages are declarative: they run once at
// load, in their own Lua states, with no `game`, no `script`, no `storage` and
// no events, and their whole job is to fill in `data.raw`. Until this existed a
// mod's data stage had to be hand-written Lua carried past the packager by
// --include, so "the mod is written in Go" had an exception in it.
//
// A data guest is a SECOND wasm module. That is measured rather than
// stylistic: under one module the control guest is parsed and instantiated at
// every data-family stage it hooks, and a real mod's control guest costs
// +150 ms of parse per game load for a program the stage never calls, while a
// data guest across all four stages is not separable from a no-module control.
// See agents/datastage.md.

// StageHook is one data-stage entry point: a guest export, the Factorio file
// that calls it, and the [stages] manifest key that orders that file.
type StageHook struct {
	// Export is the guest's //go:wasmexport name.
	Export string
	// File is the file Factorio loads, and therefore the file fklua generates.
	File string
	// Key is the [stages] key in fklua.toml that orders this stage's requires.
	Key string
	// Stage is the id handed to fk_data.lua's run(), and it is the ABI: a guest
	// compiled against fkdata.Stage() reads these numbers.
	Stage int
	// What the stage is for, printed by `fklua mod` beside the control hooks.
	What string
}

// StageHooks are the four stages a guest can hook, in load order.
//
// KEEP IN STEP WITH runtime/lua/fk_data.lua's STAGE_EXPORT table, which is what
// actually calls them. This is the same mirror factorio.Hooks is, and it is
// checked in BOTH directions for the reason that one was not: Hooks matched
// control.lua for every hook it listed and had been missing one for two
// milestones, which silently suppressed a whole class of diagnostics.
//
// THE SETTINGS FAMILY IS DELIBERATELY ONE HOOK OF ITS THREE. Factorio runs
// settings.lua, settings-updates.lua and settings-final-fixes.lua, and only
// the first is wired: nothing has needed to patch ANOTHER mod's setting the
// way data-updates patches another mod's prototypes, and wiring one is a
// one-row addition here plus a stage id the day something does. Written down
// so the omission reads as a decision rather than as a gap nobody noticed.
var StageHooks = []StageHook{
	{"fk_settings", "settings.lua", "settings", 1,
		"the settings stage: mod settings, before data.raw exists"},
	{"fk_data", "data.lua", "data", 2,
		"the data stage: prototypes"},
	{"fk_data_updates", "data-updates.lua", "data_updates", 3,
		"the data-updates stage: patch another mod's prototypes"},
	{"fk_data_final_fixes", "data-final-fixes.lua", "data_final_fixes", 4,
		"the data-final-fixes stage: the last word"},
}

// DataStageFile is the shim the generated stage files require, copied in
// verbatim like fk_abi.lua.
const DataStageFile = "fk_data.lua"

// DataModuleFile is the file the generated data chunk goes in. fk_data.lua
// requires it by this name, so the two have to agree.
const DataModuleFile = "fk_data_module.lua"

// GuestStageEntry is the token a [stages] chain uses to name the guest's own
// hook, in among the requires of whatever hand-written Lua is still there.
//
// It is a token rather than a file name because it is not a file: the hook is a
// call into a module that has already been required, and spelling it as one
// would invite an author to require it directly and get a second instance.
const GuestStageEntry = "@guest"

// StageHookByKey finds a stage by its [stages] manifest key.
func StageHookByKey(key string) (StageHook, bool) {
	for _, h := range StageHooks {
		if h.Key == key {
			return h, true
		}
	}
	return StageHook{}, false
}

// StageExportNames is the reachability root set for a DATA module's
// diagnostics, exactly as hookNames is for a control guest's.
//
// It matters for the same reason: fk_data.lua reaches a data guest through
// these four exports and no other, so a diagnostic attributed to anything else
// names a function the stage can never call -- which for a TinyGo guest means
// naming its exported libm.
func StageExportNames() []string {
	out := make([]string, 0, len(StageHooks)+1)
	// _initialize is a root too: fk_data.lua calls it before the stage hook, and
	// a TinyGo guest's package initialisers are the whole of its setup.
	out = append(out, "_initialize")
	for _, h := range StageHooks {
		out = append(out, h.Export)
	}
	return out
}

// stageFileNamed finds a stage by the file Factorio loads.
func stageFileNamed(file string) (StageHook, bool) {
	for _, h := range StageHooks {
		if h.File == file {
			return h, true
		}
	}
	return StageHook{}, false
}

// StageKeys is every [stages] key, in load order. The manifest parser and the
// flag reader both check against this, so an unknown key is refused in one
// place rather than two.
func StageKeys() []string {
	out := make([]string, 0, len(StageHooks))
	for _, h := range StageHooks {
		out = append(out, h.Key)
	}
	return out
}

// stageChains works out, for one package, which stage files to generate and
// what goes in each.
//
// THE RULES, and each one is a decision rather than a fallthrough:
//
//   - an absent key with the hook EXPORTED means ["@guest"] -- the ordinary
//     case, and it means a guest that hooks one stage needs no manifest section
//     at all;
//   - an absent key with the hook NOT exported means the file is NOT GENERATED.
//     A mod with only a data stage must not get an empty settings.lua, which is
//     the same feature-detection discipline control.lua already applies to
//     fk_on_tick and the collector triple;
//   - a key naming @guest where the hook is not exported is an ERROR at package
//     time. It is the one arrangement that is certainly a mistake: the author
//     asked for a hook the guest does not have, and the alternative is a stage
//     file that loads and does nothing;
//   - a key with no @guest is a PURE-LUA stage file, which is both the far end
//     of the migration ramp and what a data-stage-only test mod wants.
func (p *Package) stageChains() (map[string][]string, error) {
	have := map[string]bool{}
	for _, e := range p.DataExports {
		have[e] = true
	}
	out := map[string][]string{}
	for _, h := range StageHooks {
		chain, declared := p.Stages[h.Key]
		if !declared {
			if p.DataChunk != "" && have[h.Export] {
				out[h.File] = []string{GuestStageEntry}
			}
			continue
		}
		for _, entry := range chain {
			if entry != GuestStageEntry {
				continue
			}
			if p.DataChunk == "" {
				return nil, fmt.Errorf("[stages] %s names %s and this mod has no "+
					"data module; declare one with [fklua] data_module or "+
					"--data-module", h.Key, GuestStageEntry)
			}
			if !have[h.Export] {
				return nil, fmt.Errorf("[stages] %s names %s and the data module "+
					"exports no %s; either export it or take %s out of the chain",
					h.Key, GuestStageEntry, h.Export, GuestStageEntry)
			}
		}
		out[h.File] = chain
	}
	return out, nil
}

// stageFile renders one generated stage file.
//
// It is a sequence of requires, in the declared order, one entry of which may
// be the guest hook. That is all data.lua has ever been -- an ordering
// statement -- so making it explicit costs nothing and makes the mid-migration
// case ("this file, then the guest, then that file") expressible without a
// second mechanism.
//
// THE MOD'S OWN NAME RIDES ON run(), because nothing else can carry it. The
// data-stage environment has no "current mod" anywhere -- `mods` is a flat
// all-mods dictionary with no self marker, `script.mod_name` is runtime-only --
// and settings and prototypes share GLOBAL namespaces where a same-type name
// collision between two mods is silent last-writer-wins, so a guest that
// generates either needs a prefix it did not hardcode. The packager is the one
// authoritative source: it is what wrote info.json. fk_data.lua hands the name
// back through env(4).
func stageFile(h StageHook, chain []string, modName string) string {
	var b strings.Builder
	b.WriteString("-- Generated by fklua. Do not edit.\n")
	b.WriteString("--\n")
	fmt.Fprintf(&b, "-- Factorio's %s stage. The order below is [stages] %s in\n",
		h.File[:len(h.File)-len(".lua")], h.Key)
	b.WriteString("-- fklua.toml; see docs/data-stage.md in the FkLua repo.\n")
	for _, entry := range chain {
		if entry == GuestStageEntry {
			fmt.Fprintf(&b, "require(%q).run(%d, %q)\n", "fk_data", h.Stage, modName)
			continue
		}
		fmt.Fprintf(&b, "require(%q)\n", entry)
	}
	return b.String()
}

// DataWiring reports which stage hooks the data module exports and which it
// does not, so `fklua mod` can say what it connected -- the same thing Wiring
// does for the control hooks, and for the same reason: a guest that misspells
// fk_data otherwise gets a mod with no data stage and no explanation.
func (p *Package) DataWiring() (found, absent []StageHook) {
	have := map[string]bool{}
	for _, e := range p.DataExports {
		have[e] = true
	}
	for _, h := range StageHooks {
		if have[h.Export] {
			found = append(found, h)
		} else {
			absent = append(absent, h)
		}
	}
	return found, absent
}

// DataModuleImports is what a data module is allowed to import.
//
// `fkdata` is the ABI and `env` is fk_log/fk_print. ANYTHING ELSE IS REFUSED AT
// PACKAGE TIME, and the refusal is doing two jobs at once. It is the
// enforceable form of "a data guest must not import fkapi", which is otherwise
// a quiet failure -- an API pin stamp on a module nothing checks it against,
// and the runtime API's identity dragged into a stage that has no runtime. And
// it is the honest report of a harder failure underneath: fk_data.lua binds
// these two host modules and nothing else, so any other import is UNBOUND at
// instantiation, which surfaces as a mod that will not load with a message
// about a wasm module name.
var DataModuleImports = map[string]bool{"fkdata": true, "env": true}

// CheckDataModule refuses a data module that is really a control guest.
//
// THREE properties. The import set is the direct one. The `fk_api_pin_` export
// is the one that survives dead-code elimination: it is a //go:wasmexport, so
// it is a root by definition, and a guest that imports fkapi carries it whether
// or not it ever calls a member. The COLLECTOR SURFACE is the third, and it
// catches a different mistake entirely -- see collectorInDataModule.
//
// THE ORDER IS LOAD-BEARING AND IS PINNED BY A TEST: pin stamp, then collector,
// then imports. It is MOST SPECIFIC FIRST. The pin stamp names one export and
// one cause; the collector surface names up to three and one cause; the import
// check is the CATCH-ALL, right about many mistakes and specific about none
// ("imports the host module(s) X, and a data stage binds only fkdata and env").
// A module that is wrong in two ways therefore gets the sharper message first
// and the vaguer one only if it is still wrong after that fix, rather than
// being told the general thing while the exact thing was available.
func CheckDataModule(imports []string, exports []string) error {
	var bad []string
	seen := map[string]bool{}
	for _, im := range imports {
		mod := im
		if i := strings.IndexByte(im, '.'); i >= 0 {
			mod = im[:i]
		}
		if DataModuleImports[mod] || seen[mod] {
			continue
		}
		seen[mod] = true
		bad = append(bad, mod)
	}
	sort.Strings(bad)
	for _, e := range exports {
		if strings.HasPrefix(e, "fk_api_pin_") {
			return fmt.Errorf("this data module exports %s, so it imports the "+
				"generated fkapi bindings. A data stage has no runtime API -- no "+
				"game, no script, no storage, no events -- so fkapi has nothing "+
				"to reach there; import guest/go/fkdata instead", e)
		}
	}
	if err := collectorInDataModule(exports); err != nil {
		return err
	}
	if len(bad) > 0 {
		hint := ""
		for _, m := range bad {
			if m == "fk" {
				hint = " (the `fk` module is fkapi's; a data stage has no runtime " +
					"API, so import guest/go/fkdata instead)"
			}
		}
		return fmt.Errorf("this data module imports the host module(s) %s, and a "+
			"data stage binds only %s%s", strings.Join(bad, ", "),
			strings.Join(sortedKeys(DataModuleImports), " and "), hint)
	}
	return nil
}

// collectorInDataModule refuses a data module built with the guest collector.
//
// THE SAME EXPORT SET MEANS THE OPPOSITE THING HERE. For a control guest,
// CollectorSurface() is a PRECONDITION: `--gc=collected` is refused unless the
// module exports all three, because the mode is not inert without them. A data
// module is compiled `--persist=none --gc=leaking` whatever the control guest
// uses, and nothing at any data-family stage drives a collector -- there is no
// tick to pace one from and no state to survive -- so the same three exports
// say the build went wrong.
//
// IT IS EASY TO DO BY ACCIDENT IN BOTH LANGUAGES, which is why this is a
// refusal and not a comment. In Rust it takes no mistake at all beyond building
// the whole workspace at once: cargo's v2 resolver unifies `--features fk/fkgc`
// across every package in one invocation, so a control guest built collected
// carries the data crate with it. In Go it is `-gc=custom` plus the fkgc import
// left in a data guest copied from a control one.
//
// Nothing said so before, and the artefact is not free. The cost is not only
// dead code either: in Rust the feature swaps `fk`'s single #[global_allocator]
// and in Go -gc=custom routes every allocation through fkgc, so the stage runs
// under a heavier allocator it has no use for, behind a collector nothing can
// ever pace. Measured on this repository's own example data guest built both
// ways: 599,178 bytes of generated Lua against 676,282, +12.9%, both packaged
// with exit 0 and no line saying so before this check existed. To re-measure,
// which needs no game: build `-p datastage` with and without
// `--features fk/fkgc` and run `fklua compile --persist=none --gc=leaking` on
// each, that being the flag set a data module is emitted with here. Read the
// size off compile rather than off mod, since the collected arm is now refused
// by this very function; on the clean arm the two agree to the byte. The
// share depends on the guest -- the filing mod measured +3.1% on its own --
// so the message says "several percent" and cites the example for the
// figure. (The wasm sizes are not quoted anywhere, deliberately: `debug =
// "line-tables-only"` embeds the absolute build path, so they move with the
// length of the checkout's path.)
//
// The names come from CollectorSurface() rather than from a list here, for the
// reason that function's own header gives: two spellings of one export set
// drift, and the direction that drifts is always the unchecked one.
func collectorInDataModule(exports []string) error {
	have := make(map[string]bool, len(exports))
	for _, e := range exports {
		have[e] = true
	}
	var found []string
	for _, name := range CollectorSurface() {
		if have[name] {
			found = append(found, name)
		}
	}
	if len(found) == 0 {
		return nil
	}
	// BOTH SIDES NAMED, THEN THE REMEDY PER LANGUAGE, which is the shape the
	// --gc refusals use: the two facts a mismatch is made of are what the
	// artefact is and what the thing being built has to be, and a message that
	// gives one without the other sends a reader looking for a flag they may
	// never have typed.
	return fmt.Errorf("this data module carries a guest collector.\n"+
		"  what the module says: it exports %s, and those exports ARE the "+
		"collector as far as the host can tell -- so it was built WITH one\n"+
		"  what a data module is: compiled --persist=none --gc=leaking whatever "+
		"the control guest uses. It runs once and dies with the Lua state that "+
		"built it, and nothing at any data-family stage drives a collector -- "+
		"there is no tick to pace one from -- so what you have is a collector "+
		"nothing can ever pace, behind an allocator the stage does not need, and "+
		"the game parses all of it at every load. Several percent of generated "+
		"Lua: +12.9%% on this repository's own example data guest\n"+
		"Build the DATA module without one:\n"+
		"  Go:   -gc=leaking rather than -gc=custom, and no fkgc import in the "+
		"data guest's own package.\n"+
		"  Rust: build the data crate in its OWN cargo invocation, with no "+
		"--features fk/fkgc. Cargo's v2 resolver unifies features across every "+
		"package built in one invocation, so a control guest built collected in "+
		"the same command turns the collector on here too.\n"+
		"The control guest is unaffected either way: `gc` in fklua.toml and "+
		"--gc on the command line describe it alone.",
		strings.Join(found, ", "))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
