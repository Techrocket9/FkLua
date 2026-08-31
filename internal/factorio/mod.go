// Package factorio packages generated Lua as a mod Factorio will load.
//
// A mod is a directory (or zip) whose name is "<name>_<version>", containing an
// info.json -- and that really is the only file Factorio insists on. A mod with
// a control.lua is a running program, a mod with only a data stage is a set of
// prototypes, and both are ordinary. Everything here is about satisfying that
// shape exactly: Factorio's loader is unforgiving, and its complaint about a
// malformed mod arrives at game start rather than at package time, which is a
// bad place to learn about a typo in a version string.
package factorio

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	luart "github.com/Techrocket9/fklua/runtime"
)

// Info is a mod's info.json. The field order here is the order Factorio's own
// documentation lists them in, which is also the order they are written.
type Info struct {
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	Title           string   `json:"title"`
	Author          string   `json:"author"`
	FactorioVersion string   `json:"factorio_version"`
	Description     string   `json:"description,omitempty"`
	Dependencies    []string `json:"dependencies,omitempty"`
}

// DefaultFactorioVersion is the major.minor Factorio series a packaged mod
// declares. It is deliberately not the exact installed build: info.json takes a
// series, and naming a patch release there makes the mod unloadable.
//
// THAT SENTENCE IS NOW A REFUSAL AND NOT ONLY A WARNING. Validate holds the
// value to seriesRE, so a `factorio_version` naming a build is refused at
// package time with the series to write instead; it used to be written into
// info.json verbatim and discovered by the loader.
//
// It is DERIVED from DefaultAPIVersion rather than written out beside it, and
// that is this repo's own named failure shape being avoided rather than a
// tidy-up: two constants that must agree about one manifest key is exactly what
// `gc` was, and then `api`, and both times the two disagreed for milestones
// with nothing to say so. A mod whose bindings come from a 2.1.17 description
// and whose info.json declares "2.0" is refused by the loader at game start —
// the worst place to learn it — and the reverse pairing loads and then calls
// members by ids the running engine numbers differently. One source, so the
// question cannot be asked twice and answered two ways.
//
// IT IS A DEFAULT AND NOT A DERIVATION, and the distinction is what makes the
// in-game gates possible at all. This key is a statement about the ENGINE a mod
// will run on, and the pin is a statement about the DESCRIPTION it was built
// from; they are separate axes, and the series is the default only because a
// mod built against a description usually runs on that description's series.
// They come apart in exactly the case this repo is in: the committed default is
// GA (2.0.x) and the machine that runs the in-game gates has 2.1.17 installed,
// which REFUSES a mod declaring "2.0" -- "Incompatible Factorio version
// (current: 2.1, required: 2.0)", at game start, which is where every one of
// those gates would have died. So the key is overridable, by `[mod]
// factorio_version` and by `--factorio-version`, and every script under
// scripts/ passes the INSTALLED engine's series rather than inheriting this.
//
// A mod author who ships to players wants this default and should not think
// about it. A 2.1 engine loads a mod declaring 2.1 only, so a 2.0-declared mod
// with a 2.0 pin is the GA shipping shape and is what `fklua init` writes.
var DefaultFactorioVersion = majorMinor(DefaultAPIVersion)

// majorMinor takes the "2.1" series out of a "2.1.17" build id. A string with
// fewer than two dotted components is returned unchanged: there is no series to
// recover, and inventing one would be a guess.
//
// The odd value it passes through does NOT reach the loader any more. Validate
// holds every factorio_version to seriesRE, so an unrecoverable string is
// refused at package time -- which is also why this function is what Validate
// asks for the remedy to suggest: one rule about where a series stops, used by
// the thing that derives one and by the thing that refuses one.
func majorMinor(version string) string {
	first := strings.IndexByte(version, '.')
	if first < 0 {
		return version
	}
	second := strings.IndexByte(version[first+1:], '.')
	if second < 0 {
		return version
	}
	return version[:first+1+second]
}

// GeneratedModuleFile is the file the generated chunk goes in. control.lua
// requires it by this name, so the two have to agree.
const GeneratedModuleFile = "fk_module.lua"

// ABIFile is the hand-written handle table and dispatcher, copied in verbatim.
const ABIFile = "fk_abi.lua"

// APIFile is the generated member table. control.lua requires it
// unconditionally, so it is written even when a guest calls nothing -- a
// guarded require would swallow a real syntax error as "absent".
const APIFile = "fk_api_gen.lua"

// emptyAPITable is what a guest that never reaches the API ships: the file has
// to exist and parse, and there is nothing in it.
const emptyAPITable = `-- Generated by fklua. This guest calls no API members.
return { api_version = 0, application_version = "", members = {} }
`

// Factorio identifies a mod by the directory name "<name>_<version>", so the
// name cannot contain the separator or anything the loader will not accept.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]*$`)

// Factorio requires exactly three dot-separated numbers, each at most 5 digits.
var versionRE = regexp.MustCompile(`^\d{1,5}\.\d{1,5}\.\d{1,5}$`)

// info.json's factorio_version is an ENGINE SERIES and not a build: two
// dot-separated numbers, and the loader refuses a mod naming a third.
//
// Deliberately versionRE one component shorter, because that IS the whole trap.
// The two keys sit next to each other in the same file and in the same
// manifest, one takes a triple and the other refuses one, and until this
// existed the only thing that noticed the difference was Factorio at game
// start. See Validate.
var seriesRE = regexp.MustCompile(`^\d{1,5}\.\d{1,5}$`)

// Package is one mod to write.
type Package struct {
	Info Info
	// Chunk is the generated Lua for the guest module, exactly as
	// luagen.EmitModuleWith produced it.
	//
	// EMPTY IS "no control guest", the same way an empty DataChunk is "no data
	// guest", and it is what a DATA-STAGE-ONLY mod sets. Factorio requires
	// info.json and nothing else -- a prototype-only mod is an ordinary genre,
	// not a degenerate case -- so a package with no control stage ships no
	// control.lua, no fk_module.lua and no fk_api_gen.lua. See Files().
	Chunk string
	// Exports names the guest's exported functions, used only to report which
	// event hooks were recognised.
	Exports []string
	// APITable is the generated member table, PRUNED to what this guest calls.
	// Empty renders as a table with no members, which is what a guest that
	// never touches the API needs -- control.lua requires the file
	// unconditionally, so it always has to exist.
	APITable string
	// Extra is the mod's DATA STAGE and anything else authored rather than
	// generated: data.lua, prototypes/, graphics/, locale/. Keys are
	// slash-separated paths relative to the mod root; values are BYTES, not
	// text -- most of a real mod's data stage is PNGs.
	//
	// Filled by Include, merged in Files(), so a directory and a zip carry the
	// same bytes and neither can be the path that was forgotten.
	Extra map[string]string
	// extraFrom remembers which included directory contributed each key, so a
	// collision between two of them can name both.
	extraFrom map[string]string

	// DataChunk is the generated Lua for the DATA-STAGE module, if this mod has
	// one. A second wasm module compiled from its own main package -- see
	// stage.go, and agents/datastage.md for why it is a second module rather
	// than another export of the first.
	//
	// EMPTY IS "no data guest", and everything below keys on that: with no data
	// module and no [stages], Files() returns exactly the five entries it always
	// did, byte for byte.
	DataChunk string
	// DataExports names the data module's exported functions, which is what
	// decides WHICH stage files are generated. A mod with only a data stage must
	// not get an empty settings.lua.
	DataExports []string
	// Scenarios is the [scenarios] chain per SCENARIO the mod ships: an ordered
	// list of require paths, one entry of which may be ScenarioControlEntry.
	//
	// Empty generates nothing, which is what makes the whole key free for every
	// project written before it existed. See scenario.go.
	Scenarios map[string][]string
	// Stages is the [stages] chain per stage key: an ordered list of require
	// paths, one entry of which may be GuestStageEntry.
	//
	// A DECLARED key with an empty list is different from an ABSENT one -- it
	// means a stage file with no requires at all -- so presence is what is read
	// here, never length.
	Stages map[string][]string
}

// Include adds every file under dir to the mod, keeping the tree's shape.
//
// A DATA STAGE IS THE NORMAL CASE, not an extension. Every non-trivial mod has
// one -- declarative, no runtime, nothing for a guest to compile -- and until
// this existed Files() returned exactly the five generated entries, WriteDir
// removed the target first, and --zip archived the same five. The only way to
// ship a real mod was to copy files over the output afterwards, which --zip
// cannot do at all.
//
// THE FLAG IS THE MECHANISM AND fklua.toml's [mod] data IS THE DEFAULT, the
// same shape gen-bindings settled on for `lang`: one code path, and the manifest
// feeds it rather than duplicating it.
//
// Nothing is filtered. Factorio ignores files it does not recognise, and a
// packager that quietly drops something the author put in the directory is a
// worse surprise than an extra file in the archive.
func (p *Package) Include(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("include %s: %w", dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("include %s: not a directory", dir)
	}
	if p.Extra == nil {
		p.Extra = map[string]string{}
		p.extraFrom = map[string]string{}
	}
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		// Slash-separated whatever the host separator is: a zip entry name is
		// defined to use forward slashes, and Factorio's require() resolves
		// against the mod root the same way.
		name := filepath.ToSlash(rel)
		if from, dup := p.extraFrom[name]; dup {
			return fmt.Errorf("include: %s and %s both provide %s", from, dir, name)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		p.Extra[name] = string(b)
		p.extraFrom[name] = dir
		return nil
	})
}

// Validate reports anything Factorio's loader would reject, at package time
// rather than at game start.
func (p *Package) Validate() error {
	if !nameRE.MatchString(p.Info.Name) {
		return fmt.Errorf("mod name %q: must start with a letter or digit and "+
			"contain only letters, digits, spaces, dashes and underscores -- "+
			"Factorio identifies a mod by the directory name %q", p.Info.Name,
			p.Info.Name+"_"+p.Info.Version)
	}
	if !versionRE.MatchString(p.Info.Version) {
		return fmt.Errorf("mod version %q: must be three dot-separated numbers, "+
			"such as 0.1.0", p.Info.Version)
	}
	if p.Info.Title == "" {
		return fmt.Errorf("mod title is empty; Factorio requires one")
	}
	if p.Info.Author == "" {
		return fmt.Errorf("mod author is empty; Factorio requires one")
	}
	// LAST BECAUSE THAT IS WHERE info.json PUTS IT -- this function walks Info's
	// fields in Info's own order, which is Factorio's documentation order, so a
	// field added to one is added to the other in the same place.
	//
	// AND IT IS CHECKED HERE, at package time, rather than at either entry
	// point, because there are two of them: `[mod] factorio_version` and
	// --factorio-version both flow into Info.FactorioVersion verbatim, and a
	// check on each is two chances to drift apart. This is the chokepoint they
	// already share, and it is where mod.name and mod.version are checked for
	// the same reason.
	//
	// The remedy is spelled by majorMinor rather than by a second rule about
	// dots here, and it is offered only when there is one to offer: "v2.0.77"
	// has no series to recover and gets the example instead.
	if !seriesRE.MatchString(p.Info.FactorioVersion) {
		remedy := fmt.Sprintf("such as %q", DefaultFactorioVersion)
		if s := majorMinor(p.Info.FactorioVersion); seriesRE.MatchString(s) {
			remedy = fmt.Sprintf("naming a patch release there makes the mod "+
				"unloadable, so drop everything after the minor and write %q", s)
		}
		return fmt.Errorf("factorio_version %q: must be two dot-separated "+
			"numbers, the engine SERIES rather than a build -- %s. Factorio "+
			"refuses a mod whose series it cannot match at game start, which "+
			"is the worst place to learn about a two-character string",
			p.Info.FactorioVersion, remedy)
	}
	return nil
}

// DirName is the directory (or zip) name Factorio expects.
func (p *Package) DirName() string { return p.Info.Name + "_" + p.Info.Version }

// Files renders the whole mod as a name-to-contents map, so the same bytes go
// into a directory and into a zip.
func (p *Package) Files() (map[string]string, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	info, err := json.MarshalIndent(p.Info, "", "  ")
	if err != nil {
		return nil, err
	}
	files := map[string]string{"info.json": string(info) + "\n"}
	// THE CONTROL STAGE, gated on there being one -- and it is gated the same
	// way the data stage below is, on its chunk being empty.
	//
	// A package WITH a control guest produces exactly the five entries it always
	// did and exactly the bytes it always did. That is not a courtesy: it is
	// every mod ever built with fklua, and a packager that started adding or
	// dropping entries would break them all at once.
	// TestAProjectWithNoDataModuleIsByteIdentical is the gate, and it costs
	// nothing.
	//
	// A package WITHOUT one is a data-stage-only mod, which Factorio loads
	// perfectly happily: info.json is the only file it insists on. control.lua,
	// fk_module.lua and fk_api_gen.lua are the three that describe a running
	// program, and none of them has anything to say about a mod that is
	// declarative from end to end.
	if p.Chunk != "" {
		api := p.APITable
		if api == "" {
			api = emptyAPITable
		}
		files["control.lua"] = luart.ModGlue()
		files[APIFile] = api
		files[GeneratedModuleFile] = wrapChunk(p.Chunk)
	}
	// THE DATA STAGE, and everything about it is gated on there being one.
	if p.DataChunk != "" {
		files[DataStageFile] = luart.DataStage()
		files[DataModuleFile] = wrapDataChunk(p.DataChunk)
	}
	// fk_abi.lua IS NOT A CONTROL-STAGE FILE, and reading it as one produces a
	// mod that will not load. fk_data.lua opens with `require("fk_abi")` -- it
	// needs the tier-2 codec to hand a prototype table across the boundary, and
	// nothing else in it belongs to the control stage -- so a data-stage-only
	// package ships the ABI and a package with neither stage ships nothing.
	// TestTheDataStageShimRequiresTheABI reads the shim rather than a memorised
	// list, so the day fk_data.lua stops requiring it, the rule moves with it.
	if p.Chunk != "" || p.DataChunk != "" {
		files[ABIFile] = luart.ABI()
	}
	chains, err := p.stageChains()
	if err != nil {
		return nil, err
	}
	for _, h := range StageHooks {
		chain, ok := chains[h.File]
		if !ok {
			continue
		}
		files[h.File] = stageFile(h, chain)
	}
	// THE SCENARIOS, gated on there being any, so a package without the key emits
	// exactly the entries it always did.
	scen, err := p.scenarioFiles()
	if err != nil {
		return nil, err
	}
	for name, body := range scen {
		files[name] = body
	}
	// A COLLISION IS AN ERROR, and neither precedence is defensible. Letting
	// an included file win produces a mod whose guest never runs; letting the
	// generated one win produces a mod whose data stage is silently not the one
	// the author wrote. Both are discovered in game, and this is discovered at
	// package time.
	//
	// Sorted, so the message names the same file every run rather than whichever
	// one Go's map iteration reached first.
	var clashes []string
	for name := range p.Extra {
		if _, taken := files[name]; taken {
			clashes = append(clashes, name)
		}
	}
	if len(clashes) > 0 {
		sort.Strings(clashes)
		// A STAGE FILE COLLIDING IS THE MID-MIGRATION CASE AND HAS ITS OWN
		// REMEDY. A mod moving its data stage into Go lands in phases, and while
		// it does, the included tree still carries data.lua under exactly the
		// name this now generates. "Rename it" is the wrong advice there and
		// [stages] is the right one: it puts the hand-written file back in the
		// chain, in an order the author states, and the migration finishes by
		// the lists going empty and the section being deleted.
		for _, c := range clashes {
			h, isStage := stageFileNamed(c)
			if !isStage {
				continue
			}
			return nil, fmt.Errorf("included file %s would overwrite the stage "+
				"file fklua generates for %s. That is the halfway house of a mod "+
				"moving its data stage into Go, and [stages] in fklua.toml is the "+
				"way through it: rename the hand-written file (to stages/%s, say) "+
				"and name it in the chain beside the guest --\n"+
				"    [stages]\n    %s = [\"stages.%s\", %q]\n"+
				"-- then delete the entry when the Lua is gone",
				c, h.What, c, h.Key, strings.TrimSuffix(c, ".lua"), GuestStageEntry)
		}
		// ...AND A SCENARIO SHIM COLLIDING IS THE SAME HALFWAY HOUSE ONE
		// DIRECTORY OVER: a mod whose scenario already carries a hand-written
		// control.lua and has just declared the key. [scenarios] is the way
		// through it for [stages]' reason -- the hand-written file goes back into
		// the chain in an order the author states.
		for _, c := range clashes {
			if !strings.HasPrefix(c, ScenarioDir+"/") ||
				!strings.HasSuffix(c, "/control.lua") {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(c, ScenarioDir+"/"),
				"/control.lua")
			return nil, fmt.Errorf("included file %s would overwrite the scenario "+
				"shim fklua generates for %q. Rename the hand-written file (to "+
				"%s/%s/scenario.lua, say) and name it in the chain beside the mod --\n"+
				"    [scenarios]\n    %s = [%q, \"__%s__/scenario\"]\n"+
				"-- then delete the entry when the Lua is gone",
				c, name, ScenarioDir, name, name, ScenarioControlEntry, p.Info.Name)
		}
		// THE LIST IS WHAT WAS ACTUALLY WRITTEN, read back out of the map rather
		// than spelled a second time. It used to be a literal naming five files,
		// which was true of every package there was and is false of two now: a
		// data-stage-only mod has no control.lua and no fk_module.lua, and no
		// spelling of the sentence ever named fk_data.lua at all.
		var written []string
		for name := range files {
			written = append(written, name)
		}
		sort.Strings(written)
		return nil, fmt.Errorf("included file %s would overwrite a file fklua "+
			"generates; rename it or leave it out (fklua writes %s)",
			strings.Join(clashes, ", "), strings.Join(written, ", "))
	}
	for name, body := range p.Extra {
		files[name] = body
	}
	return files, nil
}

// wrapChunk turns the generated chunk into a factory.
//
// The chunk reads its host imports from `...` and ends in `return {...}`, so
// wrapping it in a vararg function makes both work through `require`, which
// gives a chunk no arguments of its own. The wrapper adds one function level
// and nothing else: the chunk's top-level locals stay locals, subject to the
// same 200-local cap they already were.
func wrapChunk(chunk string) string {
	var b strings.Builder
	b.WriteString("-- Generated by fklua. Do not edit.\n")
	b.WriteString("--\n")
	b.WriteString("-- Returns a factory: call it with the host imports table to instantiate.\n")
	b.WriteString("-- control.lua does that; see runtime/lua/fk_mod.lua in the FkLua repo.\n")
	b.WriteString("return function(...)\n")
	b.WriteString(chunk)
	if !strings.HasSuffix(chunk, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("end\n")
	return b.String()
}

// wrapDataChunk is wrapChunk for the data module, and it exists separately for
// the one line that differs: the header names the file that instantiates it.
//
// The wrapping itself is identical and has to be, because the FACTORY SHAPE is
// what makes a generated chunk loadable at a data stage at all -- `require`
// gives a chunk no arguments, and the chunk reads its host imports from `...`,
// so instantiation stays entirely under the caller's control. That was probed
// before any of this was designed: a real mod's control module requires,
// builds and initialises cleanly at the settings, data and final-fixes stages,
// demanding nothing those stages do not have.
func wrapDataChunk(chunk string) string {
	var b strings.Builder
	b.WriteString("-- Generated by fklua. Do not edit.\n")
	b.WriteString("--\n")
	b.WriteString("-- The DATA-STAGE guest module. Returns a factory: call it with the host\n")
	b.WriteString("-- imports table to instantiate. " + DataStageFile + " does that, once per\n")
	b.WriteString("-- stage; see runtime/lua/fk_data.lua in the FkLua repo.\n")
	b.WriteString("return function(...)\n")
	b.WriteString(chunk)
	if !strings.HasSuffix(chunk, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("end\n")
	return b.String()
}

// WriteDir writes the mod as a directory under parent, returning its path.
//
// An existing directory of the same name is replaced. Repackaging after a guest
// change is the common case, and leaving stale files behind produces a mod that
// half-updates.
func (p *Package) WriteDir(parent string) (string, error) {
	files, err := p.Files()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(parent, p.DirName())
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for name, body := range files {
		// Included files carry a tree (prototypes/, locale/en/, graphics/...),
		// and the name is slash-separated by construction, so it is turned back
		// into the host separator exactly once, here.
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// WriteZip writes the mod as a zip under parent, returning its path.
//
// Every entry is prefixed with the "<name>_<version>/" directory, which
// Factorio requires: a zip whose files sit at the root is not a mod.
func (p *Package) WriteZip(parent string) (string, error) {
	files, err := p.Files()
	if err != nil {
		return "", err
	}
	// WriteDir creates its parent as a side effect of creating the mod
	// directory, so -o pointing somewhere that does not exist yet has to work
	// here too rather than failing only in zip mode.
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(parent, p.DirName()+".zip")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	w := zip.NewWriter(f)
	// Sorted, so the archive is byte-identical for identical input rather than
	// reflecting Go's map iteration order.
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e, err := w.Create(p.DirName() + "/" + name)
		if err != nil {
			return "", err
		}
		if _, err := io.WriteString(e, files[name]); err != nil {
			return "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return path, nil
}

// Hook is one guest export control.lua knows how to wire up.
type Hook struct {
	Export string
	What   string
	// Event is false for the toolchain's own entry point, which every TinyGo
	// guest has and which does not make the mod do anything on its own.
	Event bool
}

// Hooks are the exports the glue looks for. Keep in step with
// runtime/lua/fk_mod.lua: an export listed here that control.lua does not
// register would be reported as wired and silently never called.
var Hooks = []Hook{
	{"_initialize", "TinyGo runtime init, run once at load", false},
	{"fk_on_init", "script.on_init", true},
	{"fk_on_tick", "script.on_event(defines.events.on_tick)", true},
	// The M7 event path, and it was missing here for two milestones. Absent, a
	// guest whose handlers hang off fk.subscribe -- the way M7 intends, with
	// fk_on_tick as the legacy shortcut -- had its diagnostics suppressed (this
	// list is the reachability root set `fklua mod` passes the emitter) and was
	// reported as a mod that would never run.
	{"fk_on_event", "fk.subscribe event dispatch", true},
	// One shot on the tick after fk.defer(), then unregistered again. A
	// blueprint paste is P SEPARATE dispatches in one tick, so batching cannot
	// hang off the end of a dispatch -- see the deferred-work section of
	// runtime/lua/fk_mod.lua.
	{"fk_on_deferred", "fk.defer, flushed once on the next tick", true},
	// Every n ticks, for as long as the guest keeps the period armed. The member
	// this replaces -- LuaBootstrap::on_nth_tick -- takes a Lua function, so it
	// bound green and could never fire; the documented substitute was a
	// self-re-arming fk.defer() chain costing one dispatch per tick where the
	// engine's own form costs one per n. The armed set lives in `storage`,
	// because Factorio saves no event registration. See the periodic section of
	// runtime/lua/fk_mod.lua.
	{"fk_on_nth_tick", "fk.on_nth_tick, every n ticks", true},
	// The callback seam: a console command a guest declared, or a method of a
	// remote interface it declared, invoked through a Lua closure the HOST
	// synthesised. An Event, because it is a hook a guest author writes -- and
	// one export rather than two because a command and a remote method differ
	// only in whether Factorio looks at the result. See the "Commands and remote
	// interfaces" section of runtime/lua/fk_mod.lua.
	{"fk_on_call", "fk.register: a command or a remote interface method", true},
	// The first tick after a save is LOADED, and then unregistered. Factorio's
	// own on_load cannot touch `game`, so this is what a guest rebuilding its
	// state from the world hangs off.
	{"fk_after_load", "the first tick after a save is loaded", true},
	// The collector's pacing surface, supplied by guest/go/fkgc and present only
	// in a guest built --gc=collected. Not an Event: nothing here is a hook a
	// guest author writes, and a guest that exports these has not thereby said
	// it will do anything -- an idle collector is never stepped. The steps are
	// driven from a one-shot on_tick armed by the fk.gc import; see the pacing
	// section of runtime/lua/fk_mod.lua.
	//
	// These three are ALSO a set, reachable as CollectorSurface() -- see there.
	{"fk_gc_step", "one bounded collection step, paced from a one-shot on_tick", false},
	{"fk_gc_dirty_base", "where the host writes the dirtied page numbers", false},
	{"fk_gc_dirty_cap", "how many dirtied page numbers that buffer holds", false},
	{"fk_alloc", "the host-call ABI, for a string or array crossing out", false},
	{"fk_alloc_static", "the host-call ABI, for a buffer that outlives one call", false},
	{"fk_free", "the host-call ABI, paired with fk_alloc", false},
	{"fk_scratch_base", "the host-call ABI's string scratch region", false},
	{"fk_scratch_size", "the size of the string scratch region", false},
	// The marshalling arena's outermost bracket. Optional in exactly the way
	// fk_scratch_base is: control.lua feature-detects the PAIR and a guest built
	// before they existed behaves as it did. Without them a host-INITIATED
	// dispatch -- an event, a command, a remote method -- whose payload overflows
	// the scratch region advances the guest's arena forever, because nothing on
	// the guest side made that call and so nothing brackets it.
	{"fk_arena_mark", "the host-call ABI, opening a host-initiated dispatch's bracket", false},
	{"fk_arena_release", "the host-call ABI, paired with fk_arena_mark", false},
	{"fk_migrate", "script.on_configuration_changed, when the guest build changed", true},
	// The opt-in half. fk_migrate alone is a NOTIFICATION on a fresh heap;
	// this one adopts the old build's linear memory, rodata included, which
	// only a guest with a layout stable across builds can survive.
	{"fk_migrate_adopt", "script.on_configuration_changed, and adopt the old heap", true},
	{"fk_state_version", "the guest's own state-format version, handed to fk_migrate", false},
	// The OTHER thing on_configuration_changed reports, and until 2026-08-16 no
	// guest could hear it. fk_migrate fires only when THIS mod's build stamp
	// moved; the event also fires when the mod SET changes -- a neighbour added,
	// REMOVED, or moved to another version -- when a startup setting moves, and
	// when the game version does. A mod that adopts an uninstalled incumbent's
	// entities has a once-per-save conversion hanging off exactly that, and had
	// nothing better than "the first event of the session" to hang it on.
	//
	// An Event: it is a hook a guest author writes. Dispatched unconditionally
	// whenever Factorio raises the event, AFTER finish_rebuild, and it takes no
	// arguments -- what the engine passes is a dictionary of tables, and a guest
	// wanting detail reads script.active_mods against what it saved.
	{"fk_on_configuration_changed", "script.on_configuration_changed, whenever Factorio raises it", true},
}

// ConfChangedHook is the export whose presence decides whether the packaged API
// table carries ConfigurationChangedData's layout.
//
// A NAME SPELLED ONCE, because two places spelling one export is this repo's
// most-repeated failure shape and here it would fail SILENTLY: a packager that
// mangled the name differently from the Hooks table would find no export, prune
// the layout, and hand the guest a hook with no payload for the rest of its
// life. Derived from Hooks rather than written beside it.
var ConfChangedHook = func() string {
	for _, h := range Hooks {
		if strings.HasSuffix(h.Export, "on_configuration_changed") {
			return h.Export
		}
	}
	panic("Hooks has no configuration-changed hook")
}()

// collectorPrefix marks the hooks that make up the collector's pacing surface.
// It is a prefix rather than a flag on Hook so that the list of names lives in
// exactly one place -- the Hooks table above, which is also what control.lua is
// checked against.
const collectorPrefix = "fk_gc_"

// CollectorSurface is the export set that IS the collector, as far as anything
// outside the guest can tell: control.lua binds all three or none of them
// (`if GC and E.fk_gc_step and E.fk_gc_dirty_base and E.fk_gc_dirty_cap`), and
// a module carrying them was built -gc=custom with guest/go/fkgc linked in.
//
// It exists because that is a PRECONDITION, not just a wiring question.
// `--gc=collected` gates the emitter -- the inlined 8-byte store goes back out
// of line -- and arms a write barrier whose only consumer is a collection step,
// so handing it to a guest with no collector is a pessimisation attached to
// machinery that can never run. `fklua compile`/`fklua mod` refuse that, and
// they read the names from here rather than spelling them a second time.
//
// Derived from Hooks rather than written out again, and pinned in both
// directions by TestTheCollectorSurfaceIsWhatControlLuaBinds -- a mirror
// checked one way drifts the other, which is the lesson Hooks itself is the
// standing example of.
func CollectorSurface() []string {
	var out []string
	for _, h := range Hooks {
		if strings.HasPrefix(h.Export, collectorPrefix) {
			out = append(out, h.Export)
		}
	}
	return out
}

// Wiring reports which hooks this guest exports and which it does not, so
// `fklua mod` can say what it actually connected. A guest that misspells
// fk_on_tick otherwise gets a mod that loads, does nothing, and says nothing.
func (p *Package) Wiring() (found, absent []Hook) {
	have := map[string]bool{}
	for _, e := range p.Exports {
		have[e] = true
	}
	for _, h := range Hooks {
		if have[h.Export] {
			found = append(found, h)
		} else {
			absent = append(absent, h)
		}
	}
	return found, absent
}

// Inert reports a guest that exports no event hook at all. Such a mod loads,
// runs its guest's initialisers and is then never called again, which is almost
// never what the author meant.
func (p *Package) Inert() bool {
	found, _ := p.Wiring()
	for _, h := range found {
		if h.Event {
			return false
		}
	}
	return true
}

// MigrationDir is where Factorio looks for a mod's migration files.
const MigrationDir = "migrations"

// LuaMigrations names the hand-written Lua migrations an include tree carried
// into this package, sorted.
//
// IT EXISTS TO KEEP A COUNT AUDITABLE, and that is the whole of it. A mod's own
// state is the guest's heap, and the heap is migrated by fk_migrate; a Lua
// migration is not FkLua's state-migration mechanism and will not become one.
// What the file type keeps is the status of INLINE ASSEMBLY -- permitted,
// marked, minimised, never generated -- and the mark is a line the packager
// prints, so a repository can grep its own build output for hand-written Lua
// rather than remembering to look.
//
// JSON MIGRATIONS ARE DATA AND ARE NOT COUNTED. Factorio's other migration form
// is a prototype-rename table, which is a fact about names rather than a program
// -- there is nothing there for a compiler to have replaced, and a packager that
// warned about one would be reporting an author for using the format correctly.
//
// The engine tracks a Lua migration ONCE PER SAVE BY FILENAME, so this is also
// the list whose names must not change casually; that is the author's business
// and not something to enforce here.
func (p *Package) LuaMigrations() []string {
	var out []string
	for name := range p.Extra {
		if strings.HasPrefix(name, MigrationDir+"/") &&
			strings.HasSuffix(name, ".lua") &&
			!strings.Contains(name[len(MigrationDir)+1:], "/") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
