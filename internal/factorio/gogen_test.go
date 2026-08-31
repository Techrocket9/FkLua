package factorio

import (
	"fmt"
	luart "github.com/Techrocket9/fklua/runtime"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func goBindings(t *testing.T) (GoBindings, *API) {
	t.Helper()
	a := loadTestAPI(t)
	g, err := GenerateGo(a, GenerateMembers(a), "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	return g, a
}

// Generated Go has to be Go. Parsing is cheap and catches every way a name or
// a literal can go wrong -- and the API supplies the names, so it is not ours
// to promise they are all well-behaved.
func TestGeneratedGoParses(t *testing.T) {
	g, _ := goBindings(t)
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, "fkapi.go", g.Source, parser.AllErrors); err != nil {
		t.Fatalf("generated Go does not parse: %v", err)
	}
	t.Logf("%d members bound, %d awaiting struct support, %d bytes",
		g.Emitted, g.Deferred, len(g.Source))
}

// The committed bindings must match what the generator produces. They are
// golden files so a regeneration is a reviewable diff, and a stale checkout is
// a build failure rather than a guest author finding a method missing.
//
// This is the same check `fklua gen-bindings --check` runs in CI; having it in
// `go test` too means a change to the generator fails where it was made.
func TestCommittedBindingsAreUpToDate(t *testing.T) {
	g, _ := goBindings(t)
	path := filepath.Join("..", "..", "guest", "go", "fkapi", "fkapi.go")
	have, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v -- run `fklua gen-bindings`", err)
	}
	if string(have) != g.Source {
		t.Errorf("%s is out of date; run `fklua gen-bindings`", path)
	}
}

// The nine globals are the only handles a guest can obtain without calling
// something, so they have to be there and they have to carry the ABI's fixed
// numbers. Their ORDER is a compatibility surface shared with fk_abi.lua.
func TestGeneratedGlobalsMatchTheABI(t *testing.T) {
	g, _ := goBindings(t)
	// Whitespace-normalised: go/format aligns the `=` in a var block, so
	// matching the raw text would be asserting gofmt's column choices rather
	// than the handle numbers this is about.
	flat := strings.Join(strings.Fields(g.Source), " ")
	for i, want := range []string{
		"Commands = LuaCommandProcessor{ObjectAt(1)}",
		"Game = LuaGameScript{ObjectAt(2)}",
		"Helpers = LuaHelpers{ObjectAt(3)}",
		"Prototypes = LuaPrototypes{ObjectAt(4)}",
		"Rcon = LuaRCON{ObjectAt(5)}",
		"Remote = LuaRemote{ObjectAt(6)}",
		"Rendering = LuaRendering{ObjectAt(7)}",
		"Script = LuaBootstrap{ObjectAt(8)}",
		"Settings = LuaSettings{ObjectAt(9)}",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("global %d: %q is missing", i+1, want)
		}
	}
	// And the Go list agrees with the Lua one, which is the actual contract.
	lua := luaGlobalNames(t)
	if len(lua) != len(abiGlobalNames) {
		t.Fatalf("fk_abi.lua names %d globals, Go has %d", len(lua), len(abiGlobalNames))
	}
	for i := range lua {
		if lua[i] != abiGlobalNames[i] {
			t.Errorf("global %d: Lua says %q, Go says %q", i+1, lua[i], abiGlobalNames[i])
		}
	}
}

// A Go keyword is a legal API parameter name and an illegal Go one. The API has
// parameters called `type`, so this is not hypothetical.
func TestParameterNamesAvoidGoKeywords(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"type", "type_"},
		{"range", "range_"},
		{"normal_name", "normal_name"},
		{"", "p3"},
		{"2bad", "p3"},
	} {
		if got := goParamName(tc.in, 3); got != tc.want {
			t.Errorf("goParamName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Snake case to Go export names, including the shapes the API actually has.
func TestExportNames(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"position", "Position"},
		{"allow_destroy_when_commands_fail", "AllowDestroyWhenCommandsFail"},
		{"LuaEntity", "LuaEntity"},
		{"", "X"},
	} {
		if got := exportName(tc.in); got != tc.want {
			t.Errorf("exportName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// luaGlobalNames reads M.GLOBAL_NAMES out of the ABI, so the two languages are
// compared rather than each being compared to a comment.
func luaGlobalNames(t *testing.T) []string {
	t.Helper()
	src := luart.ABI()
	i := strings.Index(src, "M.GLOBAL_NAMES = {")
	if i < 0 {
		t.Fatal("fk_abi.lua no longer declares M.GLOBAL_NAMES")
	}
	j := strings.Index(src[i:], "}")
	body := src[i+len("M.GLOBAL_NAMES = {") : i+j]
	var out []string
	for _, part := range strings.Split(body, ",") {
		p := strings.TrimSpace(strings.Trim(strings.TrimSpace(part), `"`))
		if p != "" && !strings.HasPrefix(p, "--") {
			out = append(out, p)
		}
	}
	return out
}

// A backtick must never reach the GENERATED sources.
//
// Two different hazards share the character. In this package's own source a
// backtick inside the raw strings that carry the Go preamble and the Lua
// template ends the string, and the Go compiler catches that in a second --
// though it reports "unexpected name after top level declaration" a hundred
// lines downstream, which is why it has cost three debugging sessions and
// earns a mention here even though no test can check it.
//
// The hazard a test CAN catch is a backtick arriving from data. Descriptions,
// type names and member names come from runtime-api.json, they flow into
// generated Go that is compiled separately, and nothing in this package would
// notice. Wube has not shipped one yet; this is what would say so if they did.
func TestNoBacktickReachesTheGeneratedSources(t *testing.T) {
	a, err := LoadAPI(apiPath)
	if err != nil {
		t.Fatal(err)
	}
	r := GenerateMembers(a)
	g, err := GenerateGo(a, r, "fkapi")
	if err != nil {
		t.Fatal(err)
	}
	lua, err := r.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct{ what, src string }{
		{"the generated Go package", g.Source},
		{"the generated Lua member table", lua},
	} {
		if i := strings.IndexByte(s.src, '`'); i >= 0 {
			lo := i - 60
			if lo < 0 {
				lo = 0
			}
			t.Errorf("%s contains a backtick at byte %d; it would close the raw "+
				"string that carries it:\n...%s...", s.what, i, s.src[lo:i+20])
		}
	}
}

// A DEFERRED STRUCT MUST NOT BE EMITTED AS AN EMPTY ONE.
//
// add() reserves the name in the emission order before recursing -- so a type
// reachable from itself does not spin -- and its failure path deleted the entry
// from byName and left it in order. emit() then wrote `type X struct {}` under
// the concept's real name, with a zero-size codec.
//
// TEN types shipped that way, MapGenSettings and TileBuildabilityRule among
// them, each of which has fields in the API. It is exactly the failure this
// package already has a rule against -- "one unexpressible field skips the
// whole struct rather than being quietly dropped, because a struct missing a
// field is a wrong value the guest cannot detect" -- and it was invisible
// because every member that would have used one was deferred for the same
// reason, so nothing referenced the empty type.
//
// Found by generating event payload structs, which are the first top-level
// registrations that can fail.
func TestADeferredStructIsNotEmittedAsAnEmptyType(t *testing.T) {
	a := loadTestAPI(t)
	g, err := cachedGo(t, a)
	if err != nil {
		t.Fatal(err)
	}
	// Which concepts genuinely have no fields, so an empty type is honest.
	fieldless := map[string]bool{}
	for _, c := range a.Concepts {
		if c.Type.Complex == "table" && len(c.Type.Parameters) == 0 {
			fieldless[exportName(c.Name)] = true
		}
	}
	re := regexp.MustCompile(`(?m)^type (\w+) struct \{\n\}`)
	for _, m := range re.FindAllStringSubmatch(g.Source, -1) {
		name := m[1]
		if fieldless[name] {
			continue
		}
		t.Errorf("%s is emitted with no fields; either the concept really has "+
			"none, or its layout was deferred and the name was left in the "+
			"emission order -- a guest cannot tell the difference", name)
	}
}

// AN INHERITED MEMBER HAS TO BE REACHABLE UNDER THE CHILD'S NAME.
//
// 83 of the 156 classes have a parent, and an inherited member appears in
// NEITHER the child's method list nor its attribute list -- so LuaEntity had no
// Position() and no SurfaceIndex(), which are LuaControl's. Dispatch never
// cared, because it is name-based and the handle decides the object, which is
// what made the workaround legal and undiscoverable at the same time:
// fkapi.LuaControl{Object: fkapi.ObjectAt(h)}.Position().
func TestASubclassReachesItsParentsMembers(t *testing.T) {
	a := loadTestAPI(t)
	g, err := cachedGo(t, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"func (o LuaEntity) Position() (MapPosition, error) {",
		"return LuaControl{o.Object}.Position()",
		"func (o LuaEntity) SurfaceIndex() (uint32, error) {",
	} {
		if !strings.Contains(g.Source, want) {
			t.Errorf("missing from the generated bindings:\n\t%s", want)
		}
	}
	if g.Inherited == 0 {
		t.Error("no member was forwarded at all")
	}
}

// A forwarder must never SHADOW a member the class declares itself. An override
// is a real thing in this API, and the child's own binding is the correct one --
// so a name the class already used is skipped, and the nearest ancestor wins
// among the rest.
func TestAForwarderNeverShadowsTheClassesOwnMember(t *testing.T) {
	a := loadTestAPI(t)
	g, err := cachedGo(t, a)
	if err != nil {
		t.Fatal(err)
	}
	// Two declarations of one method on one type do not compile, so the parse
	// gate would catch a duplicate -- but it would report it as a syntax
	// problem a long way from the cause. This says what it means.
	re := regexp.MustCompile(`(?m)^func \(o (\w+)\) (\w+)\(`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(g.Source, -1) {
		key := m[1] + "." + m[2]
		if seen[key] {
			t.Errorf("%s is declared twice; a forwarder shadowed a real member", key)
		}
		seen[key] = true
	}
}

// A DICTIONARY FIELD INSIDE A STRUCT, which was the last shape goStructs
// refused and the one that mattered most downstream.
//
// `goStructs.add` took scalars, structs and arrays. Five event payloads carry a
// `tags` field -- Tags is dictionary[string -> any] -- and every one of them was
// deferred whole, including on_built_entity and on_robot_built_entity, which are
// the two events a mod that builds things subscribes to. The first downstream
// consumer therefore read those payloads at hand-derived byte offsets on its
// single most important path, which is exactly the silent-when-wrong failure the
// generated structs exist to retire.
//
// The Lua side never had this gap: read_value routes K_DICT to the same
// read_array walk an array uses, so a dict inside a struct already worked on the
// wire. Only the Go generator refused.
func TestADictionaryFieldInsideAStructGenerates(t *testing.T) {
	g, _ := goBindings(t)
	if g.EventsDeferred != 0 {
		t.Errorf("%d event payloads still deferred, by reason %v; the dictionary "+
			"field was the whole remaining list", g.EventsDeferred, g.EventDeferBy)
	}
	for _, want := range []string{
		"func ReadOnBuiltEntity(",
		"func ReadOnRobotBuiltEntity(",
	} {
		if !strings.Contains(g.Source, want) {
			t.Errorf("the generated bindings have no %s", want)
		}
	}

	// The field is an ORDERED PAIR SLICE, not a Go map. A struct crosses in
	// both directions, and writing a Go map to the wire means choosing an order
	// for the pairs -- which Go deliberately randomizes. Factorio is a lockstep
	// simulation, so a per-run ordering reaches the game as a per-CLIENT
	// difference: a desync, found by players rather than by this suite. A slice
	// of pairs is deterministic by construction, which is the same reasoning
	// tier 2's own Value.Map follows.
	i := strings.Index(g.Source, "type OnBuiltEntity struct {")
	if i < 0 {
		t.Fatal("no OnBuiltEntity struct")
	}
	decl := g.Source[i : i+400]
	if !regexp.MustCompile(`Tags\s+\[\]Entry`).MatchString(decl) {
		t.Errorf("OnBuiltEntity.Tags is not an ordered pair slice:\n%s", decl)
	}
	if strings.Contains(decl, "Tags map[") {
		t.Error("a struct dictionary field must not be a Go map: iteration order " +
			"is randomized, and this value can be written back to the game")
	}
}

// A dictionary RETURN keyed by a dynamic value binds, and it binds as an
// ORDERED PAIR SLICE.
//
// Three members wanted one thing -- `game.surfaces` among them, which is why a
// guest could not enumerate surfaces and the first downstream mod probed
// indices instead. A Go map needs a COMPARABLE key and tier 2's Value holds
// slices, so the map shape was the whole blocker; the pair slice the dictionary
// FIELD work already introduced has no such requirement.
//
// The asymmetry with a comparable-keyed return staying a `map[K]V` is
// deliberate. That one is decode-only -- the host built it, the guest reads it,
// and there is no order for the guest to get wrong on the way back -- so
// nothing is bought by moving it. Here there is no choice to make.
func TestADictionaryKeyedByADynamicValueBinds(t *testing.T) {
	g, _ := goBindings(t)
	if n := g.DeferredBy["a dictionary keyed by a dynamic (tier 2) value"]; n != 0 {
		t.Errorf("%d members still defer on a dyn-keyed dictionary; the pair "+
			"slice has no comparability requirement", n)
	}
	name, ok := g.Names[fmt.Sprintf("LuaGameScript::surfaces/%d", MemberGet)]
	if !ok {
		t.Fatal("game.surfaces did not bind at all")
	}
	i := strings.Index(g.Source, "func (o LuaGameScript) "+name+"(")
	if i < 0 {
		t.Fatalf("no %s method in the generated source", name)
	}
	decl := g.Source[i : i+600]
	if !regexp.MustCompile(`\)\s+\(\[\]Entry\w+, error\)`).MatchString(decl) {
		t.Errorf("%s does not return an ordered pair slice:\n%s", name, decl)
	}
	if strings.Contains(decl, "make(map[") {
		t.Errorf("%s builds a Go map, whose key would have to be comparable:\n%s",
			name, decl)
	}
}
