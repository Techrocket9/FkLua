// Package fkdata is the guest-side half of Factorio's SETTINGS and DATA
// stages.
//
// A data guest is a second wasm module, compiled from its own main package,
// packaged beside the control guest and run by the stage files fklua generates.
// It writes ordinary Go, marks its entry points with //go:wasmexport, and
// reaches `data.raw` through the seven imports declared here:
//
//	//go:wasmexport fk_data
//	func onData() {
//		fkdata.Extend(fkdata.Obj(
//			fkdata.KVs("type", fkdata.Str("item")),
//			fkdata.KVs("name", fkdata.Str("my-item")),
//			fkdata.KVs("stack_size", fkdata.Num(50)),
//		))
//	}
//
// Build it exactly as a control guest is built, with [github.com/Techrocket9/fklua/guest/go/fk.BuildFlags],
// and package it with `fklua mod --data-module`.
//
// # The four hooks
//
// One export per stage, and fklua generates a stage file only for the exports
// the module actually has:
//
//	fk_settings           -> settings.lua
//	fk_data               -> data.lua
//	fk_data_updates       -> data-updates.lua
//	fk_data_final_fixes   -> data-final-fixes.lua
//
// # DO NOT IMPORT fkapi FROM A DATA GUEST
//
// There is no runtime API at these stages: no `game`, no `script`, no
// `storage`, no entities and no events. Importing the bindings attaches an API
// pin stamp to a module nothing checks it against and drags the runtime API's
// identity into a stage that has no runtime, and every one of its host imports
// would be unbound at instantiation. `fklua mod` refuses a data module that
// imports anything but `fkdata` and `env`, and names this.
//
// # No state survives a stage
//
// Factorio's settings stage is its own Lua state, and data, data-updates and
// data-final-fixes share one -- but `require` re-executes a file at every
// stage, so this module is instantiated FRESH for each stage it hooks. A
// package-level variable set in fk_data is zero again in fk_data_updates. The
// place to keep something between stages is data.raw, which is what Factorio's
// own stages do.
//
// # Determinism
//
// The data stage runs per client and a divergent prototype set is a JOIN
// REFUSAL, so nothing here hands a guest an iteration order it could branch on:
// [Keys] is sorted, [Mods] is sorted, and every dictionary a [Get] returns is
// sorted by key at every nesting level. That is a property of the host shim
// rather than a rule to follow -- see runtime/lua/fk_data.lua.
package fkdata

import "unsafe"

// ---------------------------------------------------------------------------
// The imports.
//
// TinyGo refuses a //go:wasmimport function as a VALUE ("cannot use an exported
// function as value"), so every one of them is wrapped in an ordinary Go
// function before anything else can touch it. That is a toolchain constraint
// rather than a style, and a guest author never sees it.
// ---------------------------------------------------------------------------

//go:wasmimport fkdata stage
func hostStage() uint32

//go:wasmimport fkdata get
func hostGet(pathp, retp uint32) uint32

//go:wasmimport fkdata set
func hostSet(pathp, valp uint32) uint32

//go:wasmimport fkdata extend
func hostExtend(valp uint32) uint32

//go:wasmimport fkdata clone
func hostClone(pathp, dstp uint32) uint32

//go:wasmimport fkdata keys
func hostKeys(pathp, retp uint32) uint32

//go:wasmimport fkdata env
func hostEnv(which, retp uint32) uint32

//go:wasmimport env fk_log
func hostLog(ptr, length uint32)

// Status is what a host call answers. Everything that is a FAILURE raises at
// the stage instead of arriving here -- see runtime/lua/fk_data.lua's header --
// so there are two.
const (
	statusOK     = 0
	statusAbsent = 1
)

// StageID says which of the four stages a call is running in. The numbers are
// the ABI.
type StageID uint32

const (
	StageSettings       StageID = 1
	StageData           StageID = 2
	StageDataUpdates    StageID = 3
	StageDataFinalFixes StageID = 4
)

// Name is the stage's name as Factorio spells its file.
func (s StageID) Name() string {
	switch s {
	case StageSettings:
		return "settings"
	case StageData:
		return "data"
	case StageDataUpdates:
		return "data-updates"
	case StageDataFinalFixes:
		return "data-final-fixes"
	}
	return "unknown"
}

// Stage is which stage is running.
//
// A guest that hooks more than one stage shares the same code, so this is how
// it tells them apart without writing three near-identical functions.
func Stage() StageID { return StageID(hostStage()) }

// Log writes a line to factorio-current.log.
//
// The only output channel a data stage has: there is no console, because there
// is no game yet.
func Log(s string) { hostLog(ptrOf(s), uint32(len(s))) }

// ---------------------------------------------------------------------------
// The value model.
//
// Tier 2's, which is the codec fk_abi.lua already has and which is measured
// loadable at both stages. NOT a generated per-prototype type model: there are
// 251 prototype types, the description that would drive a generator is
// prototype-api.json rather than runtime-api.json, and every operation here is
// a read, a write, an extend or a clone of an untyped structure.
// ---------------------------------------------------------------------------

// Tag says which field of a V carries the value. The numbers are fk_abi.lua's
// DYN_* tags and are the wire.
type Tag uint32

const (
	TagNil    Tag = 0
	TagBool   Tag = 1
	TagNumber Tag = 2
	TagString Tag = 3
	TagObject Tag = 4 // a LuaObject: never produced at a data stage
	TagArray  Tag = 5
	TagMap    Tag = 6
)

// V is one dynamic value: nil, a bool, a number, a string, an array or a map.
type V struct {
	Tag  Tag
	Bool bool
	Num  float64
	Str  string
	Arr  []V
	// Map is a SLICE of pairs rather than a Go map, for two reasons that both
	// matter. A V holds slices and so cannot be a Go map key; and Go randomizes
	// a map's iteration order by construction, which is exactly the per-client
	// order the host shim sorts everything to avoid. The pairs arrive sorted by
	// key.
	Map []KV
}

// KV is one entry of a map.
type KV struct{ Key, Val V }

// Nil is the absent value. Passing it to [Set] DELETES the key.
func Nil() V { return V{Tag: TagNil} }

// Num is a number.
func Num(f float64) V { return V{Tag: TagNumber, Num: f} }

// Str is a string.
func Str(s string) V { return V{Tag: TagString, Str: s} }

// Bool is a boolean.
func Bool(b bool) V { return V{Tag: TagBool, Bool: b} }

// Arr is an array, which is what Lua calls a table with 1..n and nothing else.
func Arr(vs ...V) V { return V{Tag: TagArray, Arr: vs} }

// Obj is a map, in the order given. Prototype fields are what this is for.
func Obj(kvs ...KV) V { return V{Tag: TagMap, Map: kvs} }

// KVs is one key and its value, for [Obj].
func KVs(k string, v V) KV { return KV{Key: Str(k), Val: v} }

// IsNil reports the absent value.
func (v V) IsNil() bool { return v.Tag == TagNil }

// Number reads a numeric value, or 0.
func (v V) Number() float64 {
	if v.Tag == TagNumber {
		return v.Num
	}
	return 0
}

// String reads a string value, or "".
func (v V) String() string {
	if v.Tag == TagString {
		return v.Str
	}
	return ""
}

// Boolean reads a boolean value, or false.
func (v V) Boolean() bool { return v.Tag == TagBool && v.Bool }

// At looks one key up in a map. The second result is false for anything that
// is not a map, and for a key the map does not have.
func (v V) At(key string) (V, bool) {
	if v.Tag != TagMap {
		return V{}, false
	}
	for _, kv := range v.Map {
		if kv.Key.Tag == TagString && kv.Key.Str == key {
			return kv.Val, true
		}
	}
	return V{}, false
}

// Len is the number of elements in an array, or of pairs in a map.
func (v V) Len() int {
	switch v.Tag {
	case TagArray:
		return len(v.Arr)
	case TagMap:
		return len(v.Map)
	}
	return 0
}

// ---------------------------------------------------------------------------
// The operations
// ---------------------------------------------------------------------------

// Get reads one value out of data.raw, at any depth.
//
//	count, ok := fkdata.Get("technology", "logistics", "unit", "count")
//
// The second result is false when the path is not there, which is a NORMAL
// answer rather than an error: "is this prototype already defined" is what a
// mod adopting another mod's entities asks on every load. Anything that really
// is a failure -- a malformed path, a value the codec cannot carry -- raises at
// the stage with the stage name and the path in the message.
//
// A dictionary comes back with its pairs SORTED BY KEY, at every nesting level.
func Get(path ...any) (V, bool) {
	p := encodePath(path)
	ret := scratch16()
	st := hostGet(p, ret)
	if st == statusAbsent {
		return V{}, false
	}
	return readDyn(ret), true
}

// Set writes one value into data.raw, at any depth.
//
//	fkdata.Set(fkdata.Num(0.25), "transport-belt", "my-belt", "speed")
//
// Setting [Nil] DELETES the key, which is not decoration: stripping a cloned
// prototype is a list of deletions, and a "write false" reading of an absent
// value would leave those fields present-and-false in the prototype.
//
// An intermediate step that is not there raises rather than being created,
// because a typo in a prototype name is a much likelier cause than a deliberate
// subtree build. Build a subtree by setting its root in one call.
func Set(value V, path ...any) {
	p := encodePath(path)
	if value.Tag == TagNil {
		hostSet(p, 0)
		return
	}
	v := scratch16()
	writeDyn(v, value)
	hostSet(p, v)
}

// Extend is data:extend: it adds prototypes.
//
//	fkdata.Extend(item, recipe, technology)
//
// Factorio's own extend is the validator. A prototype with no type or no name
// is refused by the game, by name, which is a better message than anything this
// layer could invent.
func Extend(protos ...V) {
	if len(protos) == 0 {
		return
	}
	v := scratch16()
	writeDyn(v, Arr(protos...))
	hostExtend(v)
}

// Clone deep-copies one prototype under another name, within one type.
//
//	fkdata.Clone("transport-belt", "express-transport-belt", "my-belt")
//
// THE COPY IS THE ENGINE'S OWN util.table.deepcopy, and that is the whole
// reason this is a primitive rather than a Get plus an Extend. Reading a
// prototype into the guest and writing it back re-serialises every leaf, so any
// field tier 2 cannot express, any float that does not round-trip and any key
// this value model drops would change the prototype SILENTLY while the mod
// still loads. Under a host-side clone the untouched leaves are literally the
// bytes the source shipped; measured on one real mod's four clones, 999 scalar
// leaves survive untouched and the patches that follow reach about 40 of them.
//
// Patch the copy afterwards with [Set].
func Clone(typ, from, to string) { CloneTo(typ, from, typ, to) }

// CloneTo is [Clone] across prototype types.
func CloneTo(srcType, srcName, dstType, dstName string) {
	src := encodePath([]any{srcType, srcName})
	dst := encodePath([]any{dstType, dstName})
	hostClone(src, dst)
}

// Keys is the STRING keys at a path, SORTED.
//
//	for _, name := range fkdata.Keys("transport-belt") { ... }
//
// This is the deterministic enumeration primitive, and the sort is why: the
// engine's own iteration order over data.raw is insertion order, which is a
// fact about how the mods happened to load rather than a promise this ABI may
// make. A tie broken by iteration order is a prototype set that differs between
// clients, which Factorio answers with a join refusal.
//
// Numeric keys are not string keys and are not returned; read an array with
// [Get] and use [V.Len].
func Keys(path ...any) []string {
	p := encodePath(path)
	ret := scratch16()
	if hostKeys(p, ret) == statusAbsent {
		return nil
	}
	v := readDyn(ret)
	out := make([]string, 0, len(v.Arr))
	for _, k := range v.Arr {
		if k.Tag == TagString {
			out = append(out, k.Str)
		}
	}
	return out
}

// ModEntry is one installed mod and its version.
type ModEntry struct{ Name, Version string }

// Mods is every installed mod and its version, SORTED BY NAME.
//
// A SLICE RATHER THAN A map[string]string, deliberately. A Go map's iteration
// order is randomized by construction, so a guest that enumerated one would
// produce a different prototype set on different clients -- the exact hazard
// the host's sorting exists to remove, reintroduced one layer up. Look a single
// mod up with [ModVersion].
func Mods() []ModEntry {
	v := envValue(1, &modsCache)
	out := make([]ModEntry, 0, len(v.Map))
	for _, kv := range v.Map {
		out = append(out, ModEntry{Name: kv.Key.String(), Version: kv.Val.String()})
	}
	return out
}

// ModVersion is one mod's version, and whether it is installed at all.
//
//	if _, ok := fkdata.ModVersion("space-age"); ok { ... }
func ModVersion(name string) (string, bool) {
	v, ok := envValue(1, &modsCache).At(name)
	if !ok {
		return "", false
	}
	return v.String(), true
}

// FeatureFlag reads one of the engine's feature flags, such as "space_travel"
// or "quality".
func FeatureFlag(name string) bool {
	v, ok := envValue(2, &flagsCache).At(name)
	return ok && v.Boolean()
}

// FeatureFlags is every feature flag, SORTED BY NAME.
func FeatureFlags() []KV { return envValue(2, &flagsCache).Map }

// StartupSetting reads one startup setting's VALUE, unwrapped from the
// {value = ...} table the engine keeps it in.
//
// The second result is false when there is no such setting -- and it is false
// for EVERY setting at the settings stage, where `settings` does not exist at
// all, because a mod's startup settings are not readable while they are being
// declared.
func StartupSetting(name string) (V, bool) {
	return envValue(3, &startupCache).At(name)
}

// ---------------------------------------------------------------------------
// The wire.
// ---------------------------------------------------------------------------

// The three env reads are cached, because they cannot change during a stage and
// each one crosses a whole dictionary. Nothing else here caches: data.raw is
// mutable by construction and a cached read of it would be a lie the moment the
// guest's own Set landed.
var (
	modsCache    envCache
	flagsCache   envCache
	startupCache envCache
)

type envCache struct {
	filled bool
	v      V
}

func envValue(which uint32, c *envCache) V {
	if c.filled {
		return c.v
	}
	ret := scratch16()
	hostEnv(which, ret)
	c.v = readDyn(ret)
	c.filled = true
	return c.v
}

// The wire widths, fixed by fk_abi.lua: one dynamic value, and one pair of them.
const dynW = 16
const dynPW = 32

// encodePath turns a variadic path into one tier-2 array. Strings and the Go
// integer and float types are what a path is made of; anything else is a
// programming error and is refused loudly rather than encoded as nil, because a
// nil path element resolves to "absent" and would read as a missing prototype.
func encodePath(path []any) uint32 {
	vs := make([]V, len(path))
	for i, p := range path {
		switch t := p.(type) {
		case string:
			vs[i] = Str(t)
		case int:
			vs[i] = Num(float64(t))
		case int32:
			vs[i] = Num(float64(t))
		case int64:
			vs[i] = Num(float64(t))
		case uint32:
			vs[i] = Num(float64(t))
		case float64:
			vs[i] = Num(t)
		case V:
			vs[i] = t
		default:
			panic("fkdata: a path element is a string or a number")
		}
	}
	p := scratch16()
	writeDyn(p, Arr(vs...))
	return p
}

// scratch16 hands out one 16-byte tier-2 slot.
//
// A FRESH ALLOCATION PER CALL RATHER THAN A REUSED PACKAGE BUFFER, and that is
// a decision the control ABI does not have to make. There, a reused buffer is
// wrong because dispatch is RE-ENTRANT -- Factorio raises events from inside
// the calls that cause them. Here the reason is simpler and the answer is the
// same: a Get whose result the guest still holds must not have its slot written
// over by the next Set, and the stage runs once and dies, so there is nothing
// for the allocation to accumulate into.
func scratch16() uint32 {
	b := make([]byte, dynW)
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}

func readDyn(p uint32) V {
	d := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(p))), dynW)
	switch Tag(*(*uint32)(unsafe.Pointer(&d[0]))) {
	case TagBool:
		return V{Tag: TagBool, Bool: d[8] != 0}
	case TagNumber:
		return V{Tag: TagNumber, Num: *(*float64)(unsafe.Pointer(&d[8]))}
	case TagString:
		ptr := *(*uint32)(unsafe.Pointer(&d[8]))
		n := *(*uint32)(unsafe.Pointer(&d[12]))
		if n == 0 {
			return V{Tag: TagString}
		}
		return V{Tag: TagString, Str: string(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), n))}
	case TagObject:
		// A data stage never produces one: the handle table is the control
		// stage's and fk_data.lua does not bind it. Decoded as nil rather than
		// as a handle nothing could resolve.
		return V{Tag: TagNil}
	case TagArray:
		base := uintptr(*(*uint32)(unsafe.Pointer(&d[8])))
		n := int(*(*uint32)(unsafe.Pointer(&d[12])))
		out := make([]V, n)
		for i := 0; i < n; i++ {
			out[i] = readDyn(uint32(base + uintptr(i)*dynW))
		}
		return V{Tag: TagArray, Arr: out}
	case TagMap:
		base := uintptr(*(*uint32)(unsafe.Pointer(&d[8])))
		n := int(*(*uint32)(unsafe.Pointer(&d[12])))
		out := make([]KV, n)
		for i := 0; i < n; i++ {
			e := base + uintptr(i)*dynPW
			out[i] = KV{Key: readDyn(uint32(e)), Val: readDyn(uint32(e + dynW))}
		}
		return V{Tag: TagMap, Map: out}
	}
	return V{Tag: TagNil}
}

func writeDyn(p uint32, v V) {
	d := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(p))), dynW)
	for i := range d {
		d[i] = 0
	}
	*(*uint32)(unsafe.Pointer(&d[0])) = uint32(v.Tag)
	switch v.Tag {
	case TagBool:
		if v.Bool {
			d[8] = 1
		}
	case TagNumber:
		*(*float64)(unsafe.Pointer(&d[8])) = v.Num
	case TagString:
		*(*uint32)(unsafe.Pointer(&d[8])) = ptrOf(v.Str)
		*(*uint32)(unsafe.Pointer(&d[12])) = uint32(len(v.Str))
	case TagArray:
		n := len(v.Arr)
		base := fkAlloc(uint32(n) * dynW)
		for i := range v.Arr {
			writeDyn(base+uint32(i)*dynW, v.Arr[i])
		}
		*(*uint32)(unsafe.Pointer(&d[8])) = base
		*(*uint32)(unsafe.Pointer(&d[12])) = uint32(n)
	case TagMap:
		// SORTED ON THE WAY OUT TOO, so that what a guest sends is a function of
		// what it meant rather than of the order it happened to build it in.
		// The host reads a map back the same way, and two guests that assembled
		// the same prototype differently produce the same bytes.
		kvs := make([]KV, len(v.Map))
		copy(kvs, v.Map)
		sortPairs(kvs)
		n := len(kvs)
		base := fkAlloc(uint32(n) * dynPW)
		for i := range kvs {
			e := base + uint32(i)*dynPW
			writeDyn(e, kvs[i].Key)
			writeDyn(e+dynW, kvs[i].Val)
		}
		*(*uint32)(unsafe.Pointer(&d[8])) = base
		*(*uint32)(unsafe.Pointer(&d[12])) = uint32(n)
	}
}

// sortPairs is a stable insertion sort by key.
//
// HAND-WRITTEN RATHER THAN sort.SliceStable, and that is about the toolchain
// rather than about taste: the sort package's Slice family goes through
// reflection, which drags a large slice of the reflect runtime into a guest
// whose whole job is to run once at load. A prototype has tens of fields, so an
// insertion sort is also the faster one at this size.
func sortPairs(kvs []KV) {
	for i := 1; i < len(kvs); i++ {
		x := kvs[i]
		j := i - 1
		for j >= 0 && keyLess(x.Key, kvs[j].Key) {
			kvs[j+1] = kvs[j]
			j--
		}
		kvs[j+1] = x
	}
}

// keyLess is the same total order fk_data.lua sorts with: numbers before
// strings, each in their own natural order. Stated twice, in two languages,
// because a wire both sides sort has to agree about what sorted means.
func keyLess(a, b V) bool {
	ra, rb := keyRank(a), keyRank(b)
	if ra != rb {
		return ra < rb
	}
	if ra == 1 {
		return a.Num < b.Num
	}
	return a.Str < b.Str
}

func keyRank(v V) int {
	switch v.Tag {
	case TagNumber:
		return 1
	case TagString:
		return 2
	}
	return 3
}

// ptrOf takes the address of a string's bytes. The empty string has no backing
// array, and the host reads len bytes, so zero is the right answer for it.
func ptrOf(s string) uint32 {
	if len(s) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(unsafe.StringData(s))))
}

// ---------------------------------------------------------------------------
// The allocator exports.
//
// The host needs somewhere in GUEST memory to put a string or a container it is
// handing over, and only the guest owns that address space. These are the same
// four exports fkapi has, and they are HERE rather than shared with it because a
// data guest must not import fkapi at all -- two modules exporting fk_alloc
// would be a duplicate symbol, which is the enforcement mechanism arriving for
// free.
//
// NOTHING IS EVER FREED, AND THAT IS CORRECT FOR THIS STAGE. A data guest runs
// once and dies with the Lua state that built it; the control guest's arena
// bracket exists because a mod ticks for hours. `fk_free` is exported because
// fk_abi.lua's codec pairs its own allocations with it, and it does nothing.
// ---------------------------------------------------------------------------

// pinned keeps every block the host was given reachable for as long as the
// stage lasts, because a //go:wasmexport that returns a bare address hands the
// collector nothing to trace -- and a guest built with -gc=custom would then
// reclaim memory the host is still writing into.
var pinned [][]byte

//go:wasmexport fk_alloc
func fkAlloc(n uint32) uint32 {
	if n == 0 {
		return 0
	}
	b := make([]byte, n)
	pinned = append(pinned, b)
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}

//go:wasmexport fk_free
func fkFree(uint32) {}

// The string scratch region: a fixed block the host writes returned strings
// into instead of calling fk_alloc for each one. Sound for the same reason it
// is sound in fkapi -- the lifetime is call-scoped, because the generated
// decoder copies the bytes into a Go string before the call returns.
var fkScratch [4096]byte

//go:wasmexport fk_scratch_base
func fkScratchBase() uint32 { return uint32(uintptr(unsafe.Pointer(&fkScratch[0]))) }

//go:wasmexport fk_scratch_size
func fkScratchSize() uint32 { return uint32(len(fkScratch)) }
