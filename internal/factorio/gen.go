package factorio

import (
	"fmt"
	"sort"
)

// Mapping the Factorio API's type system onto the wire.
//
// This is where 3774 members meet twelve wire kinds, and the interesting part
// is not the types that map -- it is being HONEST about the ones that do not. A
// member whose signature cannot be expressed is SKIPPED with a reason, never
// emitted as something that looks callable and is not. A guest author finding
// that a binding exists but returns nonsense is far worse served than one who
// finds it absent and can see why in the report.
//
// MEMBER IDS ARE DENSE INDICES over the generated set, and they do NOT need to
// be stable across Factorio versions. The member table is generated from the
// same API version the guest was compiled against and SHIPS IN THE SAME MOD, so
// the pair always matches. What has to degrade gracefully is a member missing
// from the RUNNING game, and that is the ERR_NO_MEMBER path in fk_abi.lua.

// Member is one entry in the generated table.
type Member struct {
	// ID is the index the guest passes to fk.call.
	ID int
	// Class and Name identify it; Class is empty for a global function.
	Class string
	Name  string
	// Kind is CALL, GET or SET, matching fk_abi.lua's constants.
	Kind int
	// HasValid reports that this member's class carries a `valid` attribute.
	//
	// It travels per member because the host cannot discover it: reading a key
	// a LuaObject does not have RAISES rather than returning nil, so probing
	// blind crashes on the 15 classes that lack it -- six of the nine globals
	// among them.
	HasValid bool
	// Optional reports that the DESCRIPTION declares this member's value may be
	// nil -- `optional_readable_attributes` in census.json is how many readable
	// attributes carry `optional: true`, LuaEntity.temperature among them,
	// present on a reactor and absent on a chest.
	//
	// It travels per member and it is a statement about the API rather than about
	// the running game, which is exactly what makes ERR_NO_MEMBER keep its
	// meaning: without it, reading an absent optional produced "no such member on
	// this Factorio version" -- the same status a member REMOVED in a point
	// release produces, so a guest could not tell the two apart. Now nil means
	// absent here and keeps meaning removed everywhere else. See fk_abi.lua's
	// M.invoke, and fklua-ports-samples finding Q4.
	//
	// READ SIDE ONLY. An optional WRITE would be the guest asking to clear the
	// attribute by sending nothing, which is a legitimate thing to want and is a
	// separate change: it needs an absent argument to mean "assign nil" rather
	// than "leave the argument out", and M.call's trailing-argument trim already
	// means the second thing.
	Optional bool
	Args     []FieldSpec
	Rets     []FieldSpec
	// TypedArgs is a SECOND argument list for the same member id, and only a
	// method whose parameter table is a discriminated union has one.
	//
	// Those methods take `args Value` -- one tier-2 map the guest builds by
	// hand out of key strings -- because a variant table is a discriminated
	// union and that is what tier 2 is for. It is also the most expensive thing
	// this ABI does: a tier-2 map is measured at 20,359 ns per GUI element
	// host-side against 6,175 for a flat block, and the difference is read_dyn
	// walking a tagged key/value pair list where a block reads a (ptr, len) at a
	// known offset. See agents/drafts/r4b-batched-gui-add.md.
	//
	// So the SHARED parameters cross as a tier-1 struct and the variant tail
	// crosses beside it as one optional tier-2 slot:
	//
	//	[{args, KindStruct, <shared parameters>}, {extra, KindDyn, optional}]
	//
	// TWO LISTS OVER ONE MEMBER ID, which is what keeps this additive: the
	// tier-2 form stays exactly as it was, nothing is renumbered, and a guest
	// already in the field is untouched. What tells the host which list to
	// decode is WHICH IMPORT the guest called -- fk.call reads Args and
	// fk.call_typed reads this. See fk_abi.lua's M.call_typed and used.go.
	TypedArgs []FieldSpec
	// Unfillable is set when the HOST can bind this member and no GUEST can ever
	// call it usefully. It carries the reason a guest generator defers on.
	//
	// The distinction from a Skip is the whole point, and it is why this is a
	// field on Member rather than a refusal in buildMethod. A skip is a member
	// the marshalling layer could not express: it never reaches the table,
	// fk.call answers ERR_NO_MEMBER, and nothing claims it exists. This is a
	// member that IS in the table, correctly marshalled, whose argument is a
	// value a wasm guest has no way to construct -- so binding it publishes a
	// green, plausible function whose every possible call is a silent no-op.
	//
	// THE FIVE ARE LuaBootstrap's on_init, on_load, on_event,
	// on_configuration_changed and on_nth_tick, at every pin this repo owns.
	// Four are harmlessly shadowed by FkLua's own hooks; on_nth_tick is not, and
	// was the one member in the whole API a guest could call, get OK from, and
	// never hear from again. The documented substitute is a self-re-arming
	// fk.Defer() chain, at up to one dispatch per tick where the engine's own
	// form costs one per N.
	//
	// GUEST-SIDE ONLY, deliberately: the host table keeps all five, so no member
	// id moves and a mod already in the field packages the table it packaged
	// before, byte for byte. What changes is that a guest naming one now fails
	// to COMPILE rather than failing to work.
	Unfillable string
}

// The member kinds, mirroring runtime/lua/fk_abi.lua.
const (
	MemberCall = 0
	MemberGet  = 1
	MemberSet  = 2
	// MemberGetEq reads a string attribute and compares it HOST-SIDE, returning
	// a bool: `entity.name == "transport-belt"` with the string never existing
	// in guest memory.
	//
	// A third KIND rather than a flag on GET, because that is what this table
	// already is -- one entry per (class, member, kind), with the layout its
	// signature implies computed once at generate time. As a kind it inherits
	// the handle resolution, the `valid` check, the pcall around the member read
	// and the ERR_NO_MEMBER path without a line of its own, and the member-id
	// scan that prunes the shipped table keeps working because the id is still
	// an ordinary i32 constant at the call site.
	//
	// WHY IT EXISTS, and it is measured downstream rather than assumed: a guest
	// subscribed with a CATEGORY filter is entered for every entity anyone
	// builds anywhere on the map, and `Name()` returns `string(b)` -- a copy,
	// necessarily, because the arena underneath is released when the call
	// returns. That is 32 B of permanent guest heap per build event under
	// -gc=leaking which no downstream discipline can remove, because the guest
	// must read the name to discover it does not want it. A predicate removes
	// the read rather than the copy.
	MemberGetEq = 3

	// MemberIndex, MemberLen and MemberSelf are Lua's three CLASS OPERATORS --
	// `obj[k]`, `#obj` and `obj(...)`. Eleven of them across the API on seven
	// classes, and until this landed no generator read Class.Operators at all:
	// they were not bound, not deferred and not counted, so `LuaChunkIterator`
	// bound three members none of which was the iterator, and `LuaInventory` had
	// no way to reach a slot. Reported by fklua-ports' resource-marker (RM1),
	// which is also where qol-research's Q2 (reading one entry of
	// force.technologies materialised all 319) and fluid-memory-storage's F-IDX
	// (inventory[1] unreachable) turned out to live: ONE gap, filed three times
	// from three sides.
	//
	// KINDS RATHER THAN A FLAG, for the reason MemberGetEq gives: this table is
	// already one entry per (class, member, kind) with the layout its signature
	// implies computed once, so a kind inherits handle resolution, the `valid`
	// check and the ERR_NO_MEMBER path without a line of its own, and the
	// member-id scan that prunes the shipped table keeps working because the id
	// is still an i32 constant at the call site.
	//
	// AND THREE KINDS RATHER THAN ONE, which is where the report needed
	// correcting. RM1 predicted a fifth kind for `call` and NO ABI change for the
	// other nine -- it read `obj[key]` as something the GET kind could already
	// carry. It cannot: every existing kind begins by resolving `obj[m.name]`,
	// and an index operator's key is an ARGUMENT rather than the member's name.
	// `#obj` is not a member read either. The correction is two more branches in
	// M.invoke, each two lines long.
	MemberIndex = 4 // obj[k]
	MemberLen   = 5 // #obj
	MemberSelf  = 6 // obj(...)

	// MemberGetHandle reads an attribute and returns the OBJECT rather than a
	// copy of what is in it. It exists for exactly one shape and closes exactly
	// one gap.
	//
	// The three operators above bound LuaCustomTable's `index` and `length` --
	// and NOTHING IN THE API RETURNS A LuaCustomTable, so neither was reachable
	// from anywhere. The description models the type structurally
	// (`{complex_type: "LuaCustomTable", key, value}`) rather than as a named
	// class, so mapType collapses it onto `dictionary` and every attribute
	// carrying one -- force.technologies, game.surfaces, nearly all of
	// LuaPrototypes; counted as custom_table_handle_members in census.json --
	// generates as a materialising dictionary read. Reading one
	// entry of force.technologies therefore copies all 319 across the boundary:
	// 14,544 bytes of guest heap for one lookup, measured by fklua-ports'
	// qol-research (Q2).
	//
	// A SECOND MEMBER OVER THE SAME ATTRIBUTE, rather than changing the first.
	// The materialising read is the right answer for iterating the whole table
	// and is what every existing guest calls; this is the right answer for a
	// point lookup, and which one a guest wants is a question about the guest.
	// The precedent is the `<Name>Into` variant one level up, with one
	// difference that decides the implementation: `Into` shares its member id
	// because the HOST does identical work and only the guest's use of the
	// returned (ptr, count) differs. This does not -- the host has to write a
	// handle where it used to write a (ptr, count) -- so it is a real member
	// with its own id, and a kind is how the ABI is told which.
	//
	// It needs no new branch in M.invoke: like MemberGet it resolves
	// `obj[m.name]`, and everything that differs is in the declared return kind,
	// which write_value has always dispatched on.
	MemberGetHandle = 7

	// MemberIndexSet is the WRITE half of MemberIndex -- `obj[k] = v`, key and
	// value both arguments -- and it is a kind for the same sentence IDX is,
	// read one direction over: MemberSet's value is its only argument and its
	// NAME comes out of this table, so it has nowhere to put a key.
	//
	// It exists because `settings.global["name"] = {value = true}` is the only
	// way a mod changes its own runtime-global setting, and until this landed
	// that gesture had no expression in the bindings at all. Filed by
	// BetterBeltBalancer, which needs it to turn its own setting on when it
	// adopts a save written before that setting existed.
	//
	// WHICH RECEIVERS ACCEPT A WRITE IS AN ALLOWLIST, and indexWriteHalf below
	// is it, with the evidence per row. A SECOND MEMBER OVER THE SAME OPERATOR
	// rather than a change to the first, which is MemberGetHandle's precedent
	// exactly: the read is the right answer for a read, the two have different
	// signatures, and which one a guest wants is a question about the guest.
	// Counted in census.json as index_setter_members for the reason kind 7 is --
	// a kind that reaches no line of the accounting is the F-IDX shape.
	MemberIndexSet = 8

	// MemberGlobalFunc is a function on NO CLASS -- the description's
	// `global_functions`, which is `log`, `localised_print` and `table_size` at
	// every pin this repo owns. Its Member carries an EMPTY Class, which is what
	// the Member struct's own field comment has said since the type existed and
	// what both binding generators skipped on ("global functions are not on a
	// class; not bound yet") for as long as there have been binding generators.
	//
	// IT EXISTS BECAUSE `log` IS THE ONLY WAY TO READ A LuaProfiler'S DURATION.
	// LuaProfiler's complete member set is add, divide, reset, restart, stop,
	// object_name, object_name_is and valid -- not one of them returns the
	// number, and the engine renders it only when the profiler is an ELEMENT of
	// a LocalisedString: `log{"", "took ", p}`. So a guest that cannot call log
	// cannot time anything and read the answer, which is what BetterBeltBalancer
	// reported: every timing figure it publishes is regexed out of
	// factorio-current.log, and porting its harness to a guest left it with no
	// way to produce the line at all.
	//
	// A KIND RATHER THAN AN IMPORT, and the decision is the one MemberGetEq
	// took. This table is already one entry per (class, member, kind) with the
	// layout its signature implies computed once at generate time, so a kind
	// inherits decode_args, the trailing-argument trim, encode_rets and the
	// member-id scan that prunes the shipped table without a line of its own.
	// The cheaper ask filed beside this one -- a `fk.log_dyn(ptr)` import --
	// would have been a seventh host import serving exactly one of the three,
	// leaving `localised_print` unreachable and `table_size` needing another.
	//
	// WHAT IT NEEDS IS LESS, NOT MORE: no handle, no `valid` check, no member
	// read. fk.call's first operand is unread for this kind and the binding
	// passes 0; the constant scan reads operand 1 and knows nothing about
	// kinds, so pruning is untouched by construction.
	//
	// This is the row `global_functions_bound` in census.json was written to
	// hold. A 0 that is WRITTEN DOWN is a decision, and this is the decision
	// coming due.
	MemberGlobalFunc = 9
)

// IsOperator reports the three kinds that are Lua metamethods rather than named
// members. They are the kinds whose `name` is documentation: nothing resolves
// `obj["index"]`, and the ABI dispatches on the kind alone.
//
// MemberIndexSet is deliberately NOT among them, though its name is
// documentation too. This predicate feeds `operators_bound`, which counts how
// many of the description's declared operators reached the member table, and a
// setter is a SECOND member over an operator that is already counted -- so
// including it would make that row read 13 of 11. Kind 7 is outside for the
// same reason and has a row of its own; so does this.
func (m Member) IsOperator() bool {
	return m.Kind == MemberIndex || m.Kind == MemberLen || m.Kind == MemberSelf
}

// indexWriteHalf says which classes' `index` operator has a write half, and it
// is an ALLOWLIST because the description declares none.
//
// AN OPERATOR CARRIES A `read_type` AND NEVER A `write_type`. That is a fact
// about the SCHEMA rather than about Factorio: the capability exists and the
// description records it in PROSE, on the operator itself or on the members
// that yield the class. So this is the move buildOperator already makes for the
// index KEY -- derive from what the description does state, write the
// derivation down once, and let a test enumerate every operator at the pin so
// that one added later fails here rather than being classified by a rule nobody
// re-read.
//
// A `false` ROW IS AS LOAD-BEARING AS A `true` ONE. The other shape -- emit a
// setter for every index operator and let the engine refuse the read-only ones
// -- is correct at runtime and publishes `inventory.Set(1, stack)` as though
// the API had such a thing. That is this repo's own "a skipped member is
// skipped, never faked" pointing at a member that would be FAKED rather than
// skipped, and a reader of the generated bindings reads them as the API's shape.
//
// The rows, with what the description says:
//
//   - LuaCustomTable -- TRUE. Its own operator prose says only "Access an
//     element of this custom table", and the members that YIELD one say the
//     rest: LuaSettings::global, LuaSettings::player_default,
//     LuaPlayer::mod_settings and LuaSettings::get_player_settings each carry
//     "individual settings can be changed by overwriting their ModSetting
//     table" -- the last with the assignment written out as an example -- and
//     LuaStyle::column_alignments says the same of an Alignment. Measured on
//     Factorio 2.1.14 by BetterBeltBalancer: the write takes effect from any
//     script context but on_init, persists through a save, stays per-save
//     rather than reaching mod-settings.dat, and raises
//     on_runtime_mod_setting_changed synchronously.
//   - LuaFluidBox -- TRUE, and the operator says so itself: "Access, SET OR
//     CLEAR a fluid box... Writing `nil` removes all fluid from the fluid box."
//     Present at the 2.0.77 GA pin and absent at 2.1, which retired the pair in
//     the fluid rework, so at that pin the row simply goes unasked.
//   - LuaGuiElement -- FALSE. "Gets children by name." A child is added with
//     LuaGuiElement::add and removed with ::destroy; assigning one has no
//     meaning the description offers.
//   - LuaInventory -- FALSE, and this is the row most likely to be
//     re-litigated. `inv[1]` yields a LuaItemStack, which is a VIEW: the way to
//     fill a slot is `inv[1].set_stack(...)`, which is bound. Nothing in the
//     description says an inventory slot may be assigned.
//   - LuaTransportLine -- FALSE, same shape and same reason; items reach a line
//     through ::insert_at and ::insert_at_back.
//
// Being wrong in the TRUE direction costs a member whose calls come back
// ERR_CALL_FAILED carrying the engine's own message -- which happens anyway,
// because writability is per RECEIVER and not per class: settings.global takes
// a write and settings.startup answers "LuaCustomTable is read only", and the
// two are the same member id. Being wrong in the FALSE direction costs a
// gesture a guest cannot make at all, which is what this entry is about. So a
// row moves on evidence, not on symmetry.
var indexWriteHalf = map[string]bool{
	"LuaCustomTable":   true,
	"LuaFluidBox":      true,
	"LuaGuiElement":    false,
	"LuaInventory":     false,
	"LuaTransportLine": false,
}

// A RENAME is what a name collision gets instead of a deferral.
//
// A class can declare a method and an attribute whose bound names coincide, and
// two members cannot share one identifier -- so one of them was dropped, and
// WHICH one was decided by emission order: methods are emitted before
// attributes, so the method won and the attribute's WRITE HALF was deferred.
// That is an accident dressed as a policy, and it is not free, because in both
// standing instances the two members are different calls:
//
//   - `LuaControl::driving` (write) puts the player in or out of a vehicle;
//     `set_driving(driving, force)` has a second parameter -- "the player will
//     be ejected and left at the position of the car if normal leave is not
//     possible". The attribute is the plain gesture and the method the forceful
//     one.
//   - `LuaPlayer::zoom_limits` (write) sets THE CURRENT CONTROLLER'S limits;
//     `set_zoom_limits(controller_type, zoom_limits)` sets ANY controller's, and
//     the description says exactly that on the attribute: "To set the zoom
//     limits of ANY controller type, not just the currently active one, use
//     LuaPlayer::set_zoom_limits."
//
// So both losers are members a guest can legitimately want, and both were
// unreachable from either language.
//
// THE WINNER STAYS THE METHOD and the loser gets a name written down here. A
// method's name is the description's own, and renaming it would put a spelling
// in the bindings that appears nowhere in Factorio's documentation; an
// attribute's bound name is already this generator's construction (`Set` plus
// the attribute), so a different construction for it costs a reader nothing.
//
// `Write<Name>` IS THE CONSTRUCTION, and it is this repo's own vocabulary rather
// than a coinage: an assignable attribute side is its WRITE HALF everywhere else
// here -- `indexWriteHalf`, "the index operator's WRITE half", `at.WriteType`.
//
// EACH ROW NAMES THE NAME IT REPLACES as well as the replacement, which is what
// makes the table self-checking without re-deriving the naming switch anywhere.
// A generator applies a row only when the name it computed really is `Was`, and
// a row whose `Was` nothing else in the class claimed is STALE -- the collision
// it was written for is gone. Both are recorded and both are gate failures.
//
// AN UNLISTED COLLISION STILL DEFERS SAFELY, under the reason it always had.
// What changes is that it is also recorded BY IDENTITY, so a pin that
// introduces one fails a gate naming the member rather than moving a count in a
// census diff.
type memberRenameRow struct {
	// WasGo and WasRust are the names the naming switch produces without this
	// table.
	WasGo, WasRust string
	// Go and Rust are the names to emit instead.
	Go, Rust string
}

var memberRename = map[string]memberRenameRow{
	// LuaControl::driving, the attribute write half, against the method
	// set_driving(driving, force).
	"LuaControl::driving/" + memberSetKind: {
		WasGo: "SetDriving", WasRust: "set_driving",
		Go: "WriteDriving", Rust: "write_driving",
	},
	// LuaPlayer::zoom_limits, the attribute write half, against the method
	// set_zoom_limits(controller_type, zoom_limits).
	"LuaPlayer::zoom_limits/" + memberSetKind: {
		WasGo: "SetZoomLimits", WasRust: "set_zoom_limits",
		Go: "WriteZoomLimits", Rust: "write_zoom_limits",
	},
}

// memberSetKind is MemberSet spelled for a map-literal key, which cannot call
// strconv. Pinned against the constant by TestEveryNameCollisionHasARow rather
// than trusted.
const memberSetKind = "2"

// MemberKey is the identity both binding generators index a member by: the
// class, the description's own name, and the kind. One function rather than a
// format string in four places, for the reason PinExport is one function.
func MemberKey(m Member) string {
	return fmt.Sprintf("%s::%s/%d", m.Class, m.Name, m.Kind)
}

// staleRenames reports memberRename rows on one class that no longer describe a
// collision, given the names that class actually emitted.
//
// TWO WAYS A ROW GOES STALE and the message says which. The name it REPLACES is
// no longer taken by anything else -- the method it was losing to was removed or
// renamed, so the loser could have its ordinary name back and the row is now
// inventing a spelling for nothing. Or the name it replaces was never emitted at
// all, which means the naming switch moved under the table and the row applied
// to a name nobody meant.
//
// Called once per class by each backend, with a per-language accessor rather
// than a copy of the row shape, so the two cannot drift on which field they
// read. `seen` holds the names that class emitted, the renamed one included.
func staleRenames(cls string, ms []Member, seen map[string]bool,
	pick func(memberRenameRow) (was, now string)) []string {
	var out []string
	for _, m := range ms {
		r, ok := memberRename[MemberKey(m)]
		if !ok {
			continue
		}
		was, now := pick(r)
		if !seen[now] {
			out = append(out, fmt.Sprintf(
				"%s: the row replaces %q with %q and %q was never emitted -- the "+
					"naming rule moved under the table", MemberKey(m), was, now, now))
			continue
		}
		if !seen[was] {
			out = append(out, fmt.Sprintf(
				"%s: the row replaces %q and nothing else on %s took that name -- "+
					"the collision it was written for is gone",
				MemberKey(m), was, cls))
		}
	}
	return out
}

// NameCollision is the deferral reason a guest generator reports for a member
// whose bound name another member of the class already took and which
// memberRename has no row for. Language-qualified at the call site, because Go
// and Rust really can collide differently -- Rust puts types and values in
// separate namespaces.
const NameCollision = " name collides with another member of the class"

// UnfillableHandler is the deferral reason both guest generators report for a
// member whose argument can be a Lua function. It is a constant because it is a
// census KEY -- `go_deferrals_by_reason` / `rust_deferrals_by_reason` are read
// by the version diff and by the gate, and two generators spelling one bucket
// differently would split the row in half silently.
const UnfillableHandler = "handler is a Lua function"

// typeCanBeAFunction reports a type that is, or can be, a Lua function.
//
// It walks the whole type rather than testing the top level, because the shape
// that matters is a UNION: LuaBootstrap's five handler-taking members declare
// `union(function, nil)`, which canonicalUnion cannot type and mapType therefore
// renders as tier 2 -- an honest encoding of a value that is only ever nil, and
// exactly why those five bound green for as long as the generators have existed.
// The three positions where a bare `function` appears (add_command's callback,
// add_interface's dictionary VALUE, get_event_handler's return) are already host
// SKIPS and never reach a guest generator, so this predicate's whole live
// population is the five -- which is the number to expect if a pin ever moves it.
func typeCanBeAFunction(t Type) bool {
	if t.Complex == "function" {
		return true
	}
	for _, sub := range []*Type{t.Value, t.Key} {
		if sub != nil && typeCanBeAFunction(*sub) {
			return true
		}
	}
	for _, o := range t.Options {
		if typeCanBeAFunction(o) {
			return true
		}
	}
	for _, v := range t.Values {
		if typeCanBeAFunction(v) {
			return true
		}
	}
	for _, p := range t.Parameters {
		if typeCanBeAFunction(p.Type) {
			return true
		}
	}
	for _, g := range t.VariantGroups {
		for _, p := range g.Parameters {
			if typeCanBeAFunction(p.Type) {
				return true
			}
		}
	}
	return false
}

// isCustomTable reports whether a type IS a LuaCustomTable at its top level.
//
// Not "contains one" and not "maps to KindDict": a plain `dictionary` maps to
// the same kind and is a Lua table with nothing behind it, so the only place
// this distinction survives is the description's own complex_type tag. mapType
// deliberately erases it -- the two marshal identically -- which is why this
// asks the description again rather than looking at the FieldSpec.
func isCustomTable(t Type) bool { return t.Complex == "LuaCustomTable" }

// Skip records a member that could not be expressed, and why.
type Skip struct {
	Class, Name string
	Reason      string
}

// OmittedField records a FIELD the generator left out of a struct, and why.
//
// This is not a Skip and the difference is the whole point. A Skip is a member
// that could not be expressed: the guest does not get it, and the census row
// says which piece of the marshalling layer would buy it back. An omission is a
// field the description says carries NOTHING -- the member is bound, every
// other field of it crosses, and what is missing is a value that was never
// going to arrive.
//
// It is recorded because AD4's lesson runs both ways. Answering a field-level
// problem at concept level is what took CollisionMask, MapGenSettings and 17
// members down; answering it at field level is right, and doing it SILENTLY is
// how a description that grows a hundred of these would look exactly like a
// description that grew none. A 0 nobody writes down is how eleven class
// operators stayed invisible for five milestones.
type OmittedField struct {
	// Owner is the concept or event whose struct the field was declared in,
	// or "" for a method's inline argument table.
	Owner string
	Field string
	// Type is the type name as the description spells it -- the ALIAS, not what
	// it resolves to. `frozen_color_lookup` is declared `ColorLookupTable` and
	// reads `nil` only after one hop, which is exactly the indirection that
	// made this defect hard to see in the JSON.
	Type   string
	Reason string
}

// Report is what a generation run produced, including everything it refused.
type Report struct {
	Members []Member
	Skipped []Skip
	// Defines is the defines table to render alongside the members, which the
	// CALLER supplies rather than GenerateMembers, because pruning it needs a
	// scan over a different import. Zero renders an empty table, which is what
	// a guest that reads no define wants.
	Defines DefineReport
	// Reasons counts skips by cause, which is the number worth watching: it
	// says which piece of the marshalling layer would buy the most coverage.
	Reasons map[string]int
	// Omitted is every field left out of a struct, deduplicated by
	// owner::field: a concept reached from forty members is one omission in the
	// description, not forty. OmittedBy counts them by reason, which is the row
	// the census carries and the version diff watches.
	Omitted   []OmittedField
	OmittedBy map[string]int
}

// Coverage is the fraction of attempted members that were expressible.
func (r Report) Coverage() float64 {
	total := len(r.Members) + len(r.Skipped)
	if total == 0 {
		return 0
	}
	return float64(len(r.Members)) / float64(total)
}

// typeMapper resolves named types and turns them into FieldSpecs.
type typeMapper struct {
	concepts map[string]*Concept
	classes  map[string]bool
	// visiting breaks reference cycles. The API has them -- LocalisedString is
	// defined in terms of itself -- and a cycle is not an error, it is a shape
	// this tier cannot express. Without this the generator recurses forever.
	visiting map[string]bool
	// owner is the stack of named concepts being resolved, so an omitted field
	// can say which struct it was declared in. A concept reached from many
	// members is resolved many times; the top of this stack is what makes the
	// dedup key stable across those.
	owner []string
	// omitted is keyed by owner::field for that dedup. Insertion order is kept
	// separately so the report is deterministic -- a map walk is not, and this
	// number lands in a committed census.
	omitted     map[string]OmittedField
	omittedKeys []string
}

func newTypeMapper(a *API) *typeMapper {
	m := &typeMapper{
		concepts: map[string]*Concept{},
		classes:  map[string]bool{},
		visiting: map[string]bool{},
		omitted:  map[string]OmittedField{},
	}
	for i := range a.Concepts {
		m.concepts[a.Concepts[i].Name] = &a.Concepts[i]
	}
	for _, c := range a.Classes {
		m.classes[c.Name] = true
	}
	return m
}

// builtinKind maps the API's primitive names onto wire kinds.
//
// `float` becomes f32, not f64. Lua has one number type, so it is tempting to
// widen everything -- but a field the API DECLARES as float holds a float, and
// f32 represents it exactly. Widening would double the field, mislead the guest
// struct about the type, and buy nothing. `number` is Lua's own generic number
// and stays f64, as does `double`.
//
// There is no int64 in the API, which is why none appears here.
var builtinKind = map[string]Kind{
	"boolean": KindBool,
	"string":  KindString,
	"double":  KindF64,
	"float":   KindF32,
	"number":  KindF64,
	"int8":    KindI8,
	"uint8":   KindU8,
	"int16":   KindI16,
	"uint16":  KindU16,
	"int32":   KindI32,
	"uint32":  KindU32,
	"uint64":  KindU64,
}

// mapType turns an API type into a wire field, or explains why it cannot.
func (m *typeMapper) mapType(t Type, depth int) (FieldSpec, error) {
	// A guest struct nested this deeply is not something the API has; hitting
	// the limit means a cycle slipped past the visiting set.
	if depth > 12 {
		return FieldSpec{}, fmt.Errorf("type nests deeper than 12")
	}

	if t.IsNamed() {
		return m.mapNamed(t.Name, depth)
	}

	switch t.Complex {
	case "type":
		return m.mapType(*t.Value, depth)

	case "array":
		e, err := m.mapType(*t.Value, depth+1)
		if err != nil {
			return FieldSpec{}, err
		}
		return FieldSpec{Kind: KindArray, Elem: &e}, nil

	case "dictionary", "LuaCustomTable":
		k, err := m.mapType(*t.Key, depth+1)
		if err != nil {
			return FieldSpec{}, err
		}
		v, err := m.mapType(*t.Value, depth+1)
		if err != nil {
			return FieldSpec{}, err
		}
		return FieldSpec{Kind: KindDict, Key: &k, Elem: &v}, nil

	case "table", "LuaStruct":
		// A table with VARIANT PARAMETER GROUPS is a discriminated union: a
		// base set of fields plus a group selected by a discriminant. That is
		// exactly what tier 2 carries, so it becomes a dynamic value rather
		// than a refusal.
		//
		// It used to be refused, on the plan's assumption that there were four
		// of these and they would be hand-written. There are four METHODS, and
		// 55 CONCEPTS -- 31 of them the Lua*EventFilter family -- which
		// together blocked 68 member entries. Hand-writing was never going to
		// reach those, and generating a Go type per variant is the same trap
		// tier 2 exists to avoid.
		//
		// The cost is real and worth stating: a caller gets a Value rather than
		// a named struct, so `create_entity` takes a tagged table instead of a
		// typed one. Available-but-untyped beats unavailable, and a hand-written
		// typed wrapper over the handful of high-traffic methods can still land
		// on top later.
		if len(t.VariantGroups) > 0 {
			return FieldSpec{Kind: KindDyn}, nil
		}
		fields, err := m.mapFields(t.Parameters, t.Attributes, depth+1)
		if err != nil {
			return FieldSpec{}, err
		}
		if len(fields) == 0 {
			return FieldSpec{}, fmt.Errorf("table has no expressible fields")
		}
		return FieldSpec{Kind: KindStruct, Struct: fields}, nil

	case "literal":
		// A bare literal outside a union is a constant, and there is nothing to
		// carry. Its Lua type decides the slot.
		switch t.Literal.(type) {
		case string:
			return FieldSpec{Kind: KindString}, nil
		case bool:
			return FieldSpec{Kind: KindBool}, nil
		default:
			return FieldSpec{Kind: KindF64}, nil
		}

	case "union":
		// A union of nothing but string literals is a string enum --
		// pure_string_enum_concepts in census.json, which is where the count
		// lives -- and crosses as its string. Compact i32 enums are tier 3 and
		// would need a value table both sides agree on; a string is correct
		// today and costs only bytes.
		allLiteral, allString := true, true
		for _, o := range t.Options {
			if o.Complex != "literal" {
				allLiteral = false
				break
			}
			if _, ok := o.Literal.(string); !ok {
				allString = false
			}
		}
		if allLiteral && allString {
			return FieldSpec{Kind: KindString}, nil
		}
		if f, ok := m.canonicalUnion(t, depth); ok {
			return f, nil
		}
		// Tier 2. A structural union has no fixed layout, so it crosses as a
		// self-describing tagged value.
		return FieldSpec{Kind: KindDyn}, nil

	case "tuple":
		return FieldSpec{}, fmt.Errorf("tuple")
	case "function":
		return FieldSpec{}, fmt.Errorf("callback")

	case "LuaLazyLoadedValue":
		// A HANDLE, and the laziness is preserved BY CONSTRUCTION rather than
		// by machinery.
		//
		// This was the last deliberately-refused runtime type, and it cost
		// exactly one event: mapFields fails a whole struct when one field
		// fails, so `on_player_setup_blueprint` -- whose `mapping` field is the
		// type's ONLY occurrence, in 2.0.77 and in 2.1.12 alike -- was skipped
		// entirely. Not bound on the host, no Go struct, no Rust struct.
		//
		// The refusal read as principled. `LuaLazyLoadedValue<T>` is a wrapper
		// the engine returns "for performance reasons", where T is constructed
		// only when `get` is called, and there is no fixed layout for a value
		// that does not exist yet. Marshalling T eagerly would have been
		// strictly WORSE than refusing, because it defeats the only reason the
		// type exists: for `mapping`, T is a dictionary of handles to every
		// entity in the blueprint, which would then be built on every blueprint
		// setup whether or not any mod ever looked at it.
		//
		// But there was never anything to marshal. The description declares
		// LuaLazyLoadedValue as an ordinary CLASS -- one method, `get`; two
		// attributes, `valid` and `object_name` -- so the type USAGE is an
		// object-handle field exactly like the other 261 class-typed event
		// fields in this pin, and it crosses the same K_HANDLE path: one
		// M.transient(v), a table store and an integer increment, with the
		// userdata itself untouched. `get` generates as an ordinary bound member
		// returning tier-2 dyn. The value is constructed if and only if the
		// guest calls Get(), which is the engine's own contract, honoured.
		//
		// t.Value -- the parameterised payload -- is deliberately NOT expanded
		// into the layout. It is DOCUMENTATION: the generators render it into
		// the field's doc comment so a reader knows what Get() yields, without
		// inventing per-site generic typing for a type that occurs once.
		//
		// LIFETIME needs no new rule, which is the other half of why this fits.
		// The API says an instance is valid only during the event it arrived in
		// and cannot be saved -- and that is already what the transient handle
		// space does to every event payload handle: released wholesale when the
		// dispatch returns. A guest that promotes one with fk_retain holds a
		// live handle over a dead LuaObject, and resolve()'s `.valid` check
		// turns that into ERR_INVALID, a status rather than a crash. Identical
		// to a retained LuaEntity, which is the precedent -- no more machinery,
		// and no less.
		//
		// `Any` used to block the other half of this: canonicalUnion mistyped it
		// as a handle, which would have generated `get` as returning an OBJECT
		// and silently mistyped every string and number it really returns. See
		// section C of canonicalUnion's comment below.
		f := FieldSpec{Kind: KindHandle, TypeName: "LuaLazyLoadedValue"}
		if t.Value != nil {
			f.LazyPayload = t.Value.String()
		}
		return f, nil
	}
	return FieldSpec{}, fmt.Errorf("complex type %q", t.Complex)
}

// canonicalUnion picks the one option a fixed layout can carry, for the two
// union families where such an option exists. Everything else is tier 2.
//
// These two families are not a guess -- they are what the API measurably
// contains. 52 concepts are structural unions and the most-referenced are all
// one of these two shapes:
//
//	MapPosition  = table{x,y}          | tuple[double, double]
//	Color        = table{r,g,b,a}      | tuple[float x4]
//	ForceID      = string | uint8      | LuaForce
//
//	A. ONE TABLE PLUS ARRAY SHORTHANDS. The table is the canonical form: it is
//	   what a read returns, and a write accepts either. Carrying only the table
//	   costs a guest nothing.
//
//	B. ONE CLASS PLUS SCALAR IDENTIFIERS. "A force, or its name, or its index."
//	   A read returns the object, so the handle is the form that must work.
//
// WHAT THIS COSTS, and it is a real cost rather than a free win: under B a
// guest can pass a force only as a handle, never as a name -- so reaching one
// by name means finding the LuaForce first. That is an ergonomic loss the
// generated bindings will have to paper over, not a correctness one, and tier 2
// removes it.
//
//	C. NEITHER, WHEN AN OPTION IS ITSELF OPEN-ENDED. `Any` is
//	   `string | boolean | number | table | LuaObject`, which matches shape B on
//	   a count -- one class, three scalars -- and is not shape B at all: the
//	   `table` arm makes it a genuine any-value union, so choosing the handle
//	   would type `remote.call` and `LuaLazyLoadedValue::get` as returning an
//	   OBJECT and silently mistype every string and number they really return.
//	   An option that maps to tier 2 therefore disqualifies the union, which is
//	   what nDyn below is for. `AnyBasic` was already tier 2 (it has no class
//	   arm) and `ForceID`, the shape B was written for, has no tier-2 arm.
//	   Found while binding LuaCustomTable's index operator, whose read type is
//	   `Any`: the operator would have been generated returning a handle.
//
// mapWriteType is mapType for an ATTRIBUTE'S WRITE HALF, and it differs in one
// clause: a union that would collapse to a single HANDLE arm crosses as tier 2
// instead.
//
// WHY THE TWO SIDES DIFFER, and it is a property of the union rather than a
// property of a member name. canonicalUnion's shape B -- one class plus scalar
// identifiers -- is right on a READ because the engine's answer is determined:
// it returns the object, and the scalar arms are ways of NAMING one on the way
// in. On a WRITE the guest chooses the arm, and collapsing removes the choice --
// so where the engine honours only the arm that was dropped, the member exists
// and cannot be used at all.
//
// THAT IS `LuaGuiElement::style`. It is declared `LuaStyle | string` on the
// write side and the engine accepts only the string, so the generated setter
// took an `Object` no guest could ever obtain a useful value for, and four of
// the thirteen mods the temptations survey audited restyle at runtime (one at 31
// sites). What kept it hidden is that `style` can also be set at creation time
// inside `add`'s option table.
//
// A DESCRIPTION-WIDE SCAN FINDS EXACTLY TWO writable union-with-class
// attributes at every committed pin, and the OTHER one already generated
// correctly: `LuaControl::opened` names eight classes, so `nHandle > 1`
// disqualifies shape B and it was tier 2 all along. So this rule brings `style`
// into line with `opened` rather than inventing a third behaviour -- which is
// why it is a rule over the SHAPE and not an allowlist keyed on a name. See
// indexWriteHalf for the case where an allowlist really was the answer: there
// the description states nothing at all and the evidence is prose, and here the
// description states the union and only the SIDE was being read wrongly.
//
// SCOPED TO AN ATTRIBUTE WRITE, deliberately, and not to method parameters.
// `ForceID` is shape B and appears in dozens of parameters; canonicalUnion's own
// header accepts that cost in as many words ("a guest can pass a force only as a
// handle... an ergonomic loss the generated bindings will have to paper over").
// A parameter has the engine accepting the handle and the guest able to get one,
// so nothing there is unreachable; an attribute write is the ONLY expression of
// that assignment, which is what makes this the position where the collapse can
// cost a member outright.
func (m *typeMapper) mapWriteType(t Type) (FieldSpec, error) {
	if collapsesToAHandle(m, t) {
		return FieldSpec{Kind: KindDyn}, nil
	}
	return m.mapType(t, 0)
}

// collapsesToAHandle reports a union that canonicalUnion resolves to shape B.
//
// It asks canonicalUnion rather than re-deriving the test, so the two cannot
// drift: the whole point is that this is the SAME union the read half collapses,
// answered differently because of where it sits.
func collapsesToAHandle(m *typeMapper, t Type) bool {
	for t.Complex == "type" && t.Value != nil {
		t = *t.Value
	}
	if t.Complex != "union" {
		return false
	}
	f, ok := m.canonicalUnion(t, 0)
	return ok && f.Kind == KindHandle
}

func (m *typeMapper) canonicalUnion(t Type, depth int) (FieldSpec, bool) {
	var chosenStruct, chosenHandle *FieldSpec
	nStruct, nHandle, nScalar, nShorthand, nDyn := 0, 0, 0, 0, 0

	for i := range t.Options {
		o := t.Options[i]
		// Unwrap the description-only wrapper before looking at the shape.
		for o.Complex == "type" && o.Value != nil {
			o = *o.Value
		}
		if o.Complex == "tuple" || o.Complex == "array" {
			nShorthand++
			continue
		}
		f, err := m.mapType(o, depth+1)
		if err != nil {
			return FieldSpec{}, false // anything unmappable disqualifies the union
		}
		switch f.Kind {
		case KindStruct:
			nStruct++
			c := f
			chosenStruct = &c
		case KindHandle:
			nHandle++
			c := f
			chosenHandle = &c
		case KindDyn:
			nDyn++
		default:
			nScalar++
		}
	}

	if nStruct == 1 && nShorthand > 0 && nHandle == 0 && nScalar == 0 && nDyn == 0 {
		return *chosenStruct, true
	}
	if nHandle == 1 && nScalar > 0 && nStruct == 0 && nShorthand == 0 && nDyn == 0 {
		return *chosenHandle, true
	}
	return FieldSpec{}, false
}

// nilTyped reports whether t is the description saying "there is never a value
// here": the literal type `nil`, directly or through a chain of named aliases
// for it.
//
// THE ALIAS HOP IS THE WHOLE DIFFICULTY. 2.1 declares the concept
// `ColorLookupTable` as `nil`, with the description "Does not return the value
// at runtime.", and the field that carries it -- UtilityConstants's
// `frozen_color_lookup` -- is declared `ColorLookupTable`. So a grep for a
// nil-typed FIELD finds nothing in any published version, and the one nil in
// the file is a concept definition three hundred entries away from the field it
// governs.
//
// It is deliberately NOT "the type mapper failed with the nil error somewhere
// underneath". An `array of nil` or a union with a nil arm would satisfy that
// and neither means what a bare nil field means -- an array of nothing is a
// count the guest can still read, and a nil union arm is Lua's way of spelling
// optionality, which this ABI carries in a presence byte instead. Neither shape
// occurs in any description committed here; if one appears it will arrive as a
// SKIP, loudly, in the census diff, which is the right way for a shape nobody
// has reasoned about to show up.
func (m *typeMapper) nilTyped(t Type) bool {
	// Bounded rather than cycle-tracked: this walks an alias chain, and a
	// cyclic one cannot terminate at a literal name anyway.
	for i := 0; i < 12; i++ {
		if t.Complex == "type" && t.Value != nil {
			t = *t.Value // the description-only wrapper
			continue
		}
		if !t.IsNamed() {
			return false // any complex type is a shape, not an absence
		}
		if t.Name == "nil" {
			return true
		}
		c, ok := m.concepts[t.Name]
		if !ok {
			return false
		}
		t = c.Type
	}
	return false
}

// omit records a field left out of the struct being mapped.
//
// Deduplicated by owner::field, because a concept is resolved once per member
// that mentions it and the census is counting the DESCRIPTION's fields, not the
// mapper's visits to them.
func (m *typeMapper) omit(field, typeName, reason string) {
	var owner string
	if n := len(m.owner); n > 0 {
		owner = m.owner[n-1]
	}
	key := owner + "::" + field
	if _, seen := m.omitted[key]; seen {
		return
	}
	m.omitted[key] = OmittedField{Owner: owner, Field: field, Type: typeName, Reason: reason}
	m.omittedKeys = append(m.omittedKeys, key)
}

// omissions returns what was omitted, in a deterministic order, plus the counts
// by reason.
func (m *typeMapper) omissions() ([]OmittedField, map[string]int) {
	out := make([]OmittedField, 0, len(m.omittedKeys))
	by := map[string]int{}
	for _, k := range m.omittedKeys {
		o := m.omitted[k]
		out = append(out, o)
		by[o.Reason]++
	}
	return out, by
}

func (m *typeMapper) mapNamed(name string, depth int) (FieldSpec, error) {
	if k, ok := builtinKind[name]; ok {
		return FieldSpec{Kind: k}, nil
	}
	if name == "nil" {
		return FieldSpec{}, fmt.Errorf("nil has no representation")
	}
	// `table` and `LuaObject` unqualified are "any table" and "any object" --
	// no shape to generate against.
	if name == "table" {
		// "any table" has no shape to generate against, which is the same
		// problem tier 2 solves.
		return FieldSpec{Kind: KindDyn}, nil
	}
	if name == "LuaObject" {
		return FieldSpec{Kind: KindHandle}, nil
	}
	if m.classes[name] {
		return FieldSpec{Kind: KindHandle}, nil
	}
	// defines.* are named integer constants. They cross as u32; their VALUES
	// are Factorio's and get resolved through a table at load rather than baked
	// in, because they are not stable across versions.
	if len(name) > 8 && name[:8] == "defines." {
		return FieldSpec{Kind: KindU32}, nil
	}
	c, ok := m.concepts[name]
	if !ok {
		return FieldSpec{}, fmt.Errorf("unknown type %q", name)
	}
	if m.visiting[name] {
		// LocalisedString really is defined in terms of itself. A fixed layout
		// cannot hold that, which is exactly what tier 2 is for: the tag says
		// what each level actually contains.
		return FieldSpec{Kind: KindDyn}, nil
	}
	m.visiting[name] = true
	defer delete(m.visiting, name)
	// Name the struct whose fields are about to be mapped, so an omission can
	// say where it came from.
	m.owner = append(m.owner, name)
	defer func() { m.owner = m.owner[:len(m.owner)-1] }()
	f, err := m.mapType(c.Type, depth)
	if err != nil {
		return f, err
	}
	// Remember where an aggregate came from. A scalar keeps no name: `MapTick`
	// is a uint64 and a guest wants uint64, not a one-field wrapper.
	if f.Kind == KindStruct {
		f.TypeName = name
	}
	return f, nil
}

// mapFields maps a table's parameters, or a LuaStruct's attributes.
//
// A field that cannot be expressed fails the WHOLE struct rather than being
// dropped. A struct silently missing a field is a wrong value the guest cannot
// detect; a struct that does not exist is a missing binding it can see.
//
// A `nil`-TYPED FIELD IS THE ONE EXCEPTION, and it is an exception because it
// is not the same statement. The rule above is about a field this layer cannot
// EXPRESS -- a value exists and the guest would silently not get it. A nil type
// is the description saying there is no value: `ColorLookupTable` is declared
// `nil` and described "Does not return the value at runtime.", so nothing is
// lost by leaving it out and nothing could be gained by keeping it. Carrying it
// as an always-absent optional would spend a presence byte, forever, saying no.
//
// This is AD4 read the right way round. There, one unmarshalable field was
// answered at CONCEPT level and took CollisionMask, MapGenSettings and 17
// members with it; here a field-level fact is answered at field level, and the
// concept, the attribute that returns it and every other field of it survive.
// The omission is recorded rather than silent -- see OmittedField.
func (m *typeMapper) mapFields(ps []Parameter, attrs []Attribute, depth int) ([]FieldSpec, error) {
	// `order` is the canonical field order, not the order the JSON array
	// happens to be in. Sorting by it means a regeneration from a reordered
	// dump produces the same offsets -- and offsets are what the guest struct
	// was compiled against.
	ps = append([]Parameter(nil), ps...)
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Order < ps[j].Order })
	attrs = append([]Attribute(nil), attrs...)
	sort.SliceStable(attrs, func(i, j int) bool { return attrs[i].Order < attrs[j].Order })

	var out []FieldSpec
	for _, p := range ps {
		if m.nilTyped(p.Type) {
			m.omit(p.Name, p.Type.Name, "nil")
			continue
		}
		f, err := m.mapType(p.Type, depth)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", p.Name, err)
		}
		f.Name, f.Optional = p.Name, p.Optional
		out = append(out, f)
	}
	for _, a := range attrs {
		if a.ReadType == nil {
			continue
		}
		if m.nilTyped(*a.ReadType) {
			m.omit(a.Name, a.ReadType.Name, "nil")
			continue
		}
		f, err := m.mapType(*a.ReadType, depth)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", a.Name, err)
		}
		f.Name, f.Optional = a.Name, a.Optional
		out = append(out, f)
	}
	return out, nil
}

// GenerateMembers walks the whole API and produces the member table.
func GenerateMembers(a *API) Report {
	m := newTypeMapper(a)
	r := Report{Reasons: map[string]int{}}

	skip := func(class, name string, err error) {
		r.Skipped = append(r.Skipped, Skip{Class: class, Name: name, Reason: err.Error()})
		r.Reasons[classify(err)]++
	}

	classes := append([]Class(nil), a.Classes...)
	sort.Slice(classes, func(i, j int) bool { return classes[i].Name < classes[j].Name })

	for _, c := range classes {
		hasValid := false
		for _, at := range c.Attributes {
			if at.Name == "valid" {
				hasValid = true
				break
			}
		}

		methods := append([]Method(nil), c.Methods...)
		sort.Slice(methods, func(i, j int) bool { return methods[i].Name < methods[j].Name })
		for _, meth := range methods {
			mem, err := buildMethod(m, c.Name, meth)
			if err != nil {
				skip(c.Name, meth.Name, err)
				continue
			}
			mem.HasValid = hasValid
			r.Members = append(r.Members, mem)
		}

		attrs := append([]Attribute(nil), c.Attributes...)
		sort.Slice(attrs, func(i, j int) bool { return attrs[i].Name < attrs[j].Name })
		for _, at := range attrs {
			if at.ReadType != nil {
				f, err := m.mapType(*at.ReadType, 0)
				if err != nil {
					skip(c.Name, at.Name, err)
				} else {
					// OPTIONALITY IS THE ATTRIBUTE'S, and it was dropped
					// here for as long as this generator has existed.
					// buildMethod has always carried rv.Optional onto a
					// method's return; the attribute path set only the name,
					// so every optional readable attribute
					// (`optional_readable_attributes` in census.json) was typed
					// as always-present on both sides and an absent one came
					// back as ERR_NO_MEMBER.
					f.Name, f.Optional = "value", at.Optional
					r.Members = append(r.Members, Member{
						Class: c.Name, Name: at.Name, Kind: MemberGet,
						Rets: []FieldSpec{f}, HasValid: hasValid,
						Optional: at.Optional,
					})
					// AND THE HANDLE VARIANT, for an attribute whose type
					// IS a LuaCustomTable. See MemberGetHandle: without it the
					// index and length operators bound on that class are
					// unreachable from anywhere in the API, because nothing
					// returns one.
					//
					// Gated on the TOP-LEVEL type rather than on f.Kind, because
					// KindDict is also what a plain `dictionary` maps to and a
					// plain dictionary is a Lua table with no handle behind it.
					// The distinction exists only in the description.
					if isCustomTable(*at.ReadType) {
						r.Members = append(r.Members, Member{
							Class: c.Name, Name: at.Name, Kind: MemberGetHandle,
							Rets: []FieldSpec{{Name: "value", Kind: KindHandle,
								Optional: at.Optional}},
							HasValid: hasValid, Optional: at.Optional,
						})
					}
					// AND THE PREDICATE, for a plain string attribute. See
					// MemberGetEq: the point is that the guest never receives
					// the string, so `entity.name == "x"` costs no guest heap.
					//
					// OPTIONAL STRINGS GET ONE TOO, and this guard used to
					// say `&& !f.Optional` on the reasoning that an absent
					// optional compares false, which the caller cannot
					// distinguish from "present and different".
					//
					// That reasoning stands and does not lead where it was
					// taken. The condition was DEAD -- f.Optional was never set
					// on this path, which is the defect above -- so honouring
					// it would have deleted the 30 optional string attributes'
					// predicates as a side effect of fixing something else,
					// and they work: nil is not the string, which is the honest
					// answer and the one call_eq already produces. A caller who
					// needs to tell absent from different asks the GET, which
					// now says so.
					if f.Kind == KindString {
						r.Members = append(r.Members, Member{
							Class: c.Name, Name: at.Name, Kind: MemberGetEq,
							Args:     []FieldSpec{{Name: "want", Kind: KindString}},
							Rets:     []FieldSpec{{Name: "value", Kind: KindBool}},
							HasValid: hasValid,
							// The BOOL is never absent; what may be absent is
							// the attribute behind it, and that is what lets
							// M.invoke fall through to call_eq instead of
							// reporting a missing member.
							Optional: at.Optional,
						})
					}
				}
			}
			if at.WriteType != nil {
				f, err := m.mapWriteType(*at.WriteType)
				if err != nil {
					// Only report once for an attribute whose read side already
					// failed for the same reason.
					if at.ReadType == nil {
						skip(c.Name, at.Name, err)
					}
				} else {
					f.Name = "value"
					r.Members = append(r.Members, Member{
						Class: c.Name, Name: at.Name, Kind: MemberSet,
						Args: []FieldSpec{f}, HasValid: hasValid,
					})
				}
			}
		}

		// THE OPERATORS, LAST, and last is deliberate: `seen` in both binding
		// generators is first-come, so putting them here means an operator can
		// only ever lose a name to a member the class really declares rather
		// than the other way round. TestOperatorsBindOnEveryClassThatHasOne
		// fails if one ever does lose, so the outcome is loud instead of a
		// three-member LuaChunkIterator.
		ops := append([]Operator(nil), c.Operators...)
		sort.Slice(ops, func(i, j int) bool { return ops[i].Name < ops[j].Name })
		hasLength := false
		for _, op := range ops {
			if op.Name == "length" {
				hasLength = true
			}
		}
		for _, op := range ops {
			mem, err := buildOperator(m, c.Name, op, hasLength)
			if err != nil {
				skip(c.Name, "operator "+op.Name, err)
				continue
			}
			mem.HasValid = hasValid
			r.Members = append(r.Members, mem)

			// AND THE WRITE HALF, for the classes indexWriteHalf names. A
			// second member over the same operator, like the handle variant of
			// a LuaCustomTable attribute one loop up, and for the same reason:
			// the two have different signatures and the read is still the right
			// answer for a read.
			//
			// A class with no entry gets no setter and is NOT skipped -- a skip
			// is what a member the generator tried and could not express, and
			// this one it did not try. TestEveryIndexOperatorHasAWriteVerdict is
			// what stops "no entry" being a way to forget one.
			if op.Name == "index" && indexWriteHalf[c.Name] {
				sm, err := buildIndexSetter(m, c.Name, op, hasLength)
				if err != nil {
					skip(c.Name, "operator "+op.Name+" (write)", err)
					continue
				}
				sm.HasValid = hasValid
				r.Members = append(r.Members, sm)
			}
		}
	}

	// THE GLOBAL FUNCTIONS, AFTER EVERY CLASS, and after is a decision rather
	// than the loop's leftovers. Member ids are dense indices into this slice,
	// so a member inserted anywhere else renumbers everything below it and the
	// golden diff becomes 8,000 lines of moved constants with the real change
	// somewhere in the middle. Appending leaves every existing id where it was:
	// at the 2.0.77 pin these are 4260, 4261 and 4262, and nothing before them
	// moved by one.
	//
	// Sorted by NAME, like the classes and like every member loop above, so a
	// regeneration is a no-op. The description's own order is by `order` and is
	// not the one anything else here reads.
	//
	// A GLOBAL FUNCTION IS A METHOD WITH NO RECEIVER, so buildMethod does the
	// whole job -- the parameter walk, the return walk, the takes_table and
	// variant-group branches -- and the only thing that differs is the kind and
	// the empty class. HasValid stays false because there is no object to have a
	// `valid` attribute.
	gfns := append([]Method(nil), a.GlobalFunctions...)
	sort.Slice(gfns, func(i, j int) bool { return gfns[i].Name < gfns[j].Name })
	for _, gf := range gfns {
		mem, err := buildMethod(m, "", gf)
		if err != nil {
			skip("", gf.Name, err)
			continue
		}
		mem.Kind = MemberGlobalFunc
		r.Members = append(r.Members, mem)
	}

	for i := range r.Members {
		r.Members[i].ID = i + 1 // 1-based, matching the Lua table
	}
	r.Omitted, r.OmittedBy = m.omissions()
	return r
}

// ---------------------------------------------------------------------------
// String-literal unions
//
// Some named concepts are a union of nothing but string literals --
// pure_string_enum_concepts in census.json counts them, and WaitConditionType
// and LinkedGameControl are the two large ones -- and mapType flattens every
// one of them
// to KindString, which is the right WIRE answer and throws the names away. What
// reaches a guest author is a bare `String` on the one field a schedule record
// cannot be written without, so `"inactivty"` compiles, packages, loads, and
// produces a schedule the engine silently rejects: a mod that never refuels
// anything. fklua-ports' fuel-train-stop reported it as FTS2 and stood a TEST in
// for the type; resource-marker confirmed it on SignalID.Type.
//
// CONSTANTS RATHER THAN AN ENUM, in both languages, and the reason is the same
// one in each: the value has to stay a string. It crosses the wire as a string,
// every generated field holding one is typed as a string, and a guest that
// already passes string literals must keep compiling. An enum would be a better
// type and a breaking change to every call site, for a union the API can extend
// in a point release -- which is what `#[non_exhaustive]` exists to admit and
// what a plain constant does not have to.
// ---------------------------------------------------------------------------

// LiteralUnion is one named concept that is a union of string literals.
type LiteralUnion struct {
	Name     string
	Literals []string
}

// StringLiteralUnions finds them, in the order the description declares, which
// is the order both backends emit so a regeneration is a no-op.
func StringLiteralUnions(a *API) []LiteralUnion {
	var out []LiteralUnion
	for _, c := range a.Concepts {
		t := c.Type
		if t.Complex != "union" || len(t.Options) == 0 {
			continue
		}
		lits := make([]string, 0, len(t.Options))
		for _, o := range t.Options {
			s, ok := o.Literal.(string)
			if o.Complex != "literal" || !ok {
				lits = nil
				break
			}
			lits = append(lits, s)
		}
		if len(lits) == 0 {
			continue
		}
		out = append(out, LiteralUnion{Name: c.Name, Literals: lits})
	}
	return out
}

// punctName transliterates ASCII punctuation into an identifier fragment.
//
// Eleven of ArithmeticCombinatorParameterOperation's options are `*`, `/`, `%`,
// `>>` and the like, and ComparatorString is `=`, `>`, `<=`, so the obvious
// name-from-the-literal rule produces the empty string for all of them at once.
// This is a TRANSLITERATION and not a naming scheme -- each symbol gets the name
// it has in every programming language's lexer -- so that the constants for the
// operators a typo actually hurts exist at all. Anything left without a name is
// skipped and counted rather than given a positional one, which would be a
// constant whose identifier says nothing.
var punctName = map[rune]string{
	'*': "Star", '/': "Slash", '+': "Plus", '-': "Minus", '%': "Percent",
	'^': "Caret", '<': "Lt", '>': "Gt", '=': "Eq", '!': "Bang", '&': "Amp",
	'|': "Pipe", '~': "Tilde", '≥': "Ge", '≤': "Le", '≠': "Ne", '#': "Hash",
	'@': "At", '?': "Question", '.': "Dot", ',': "Comma", ':': "Colon",
	';': "Semi", '$': "Dollar",
}

// LiteralIdent turns one literal into an identifier fragment, or reports that
// it has no name. Shared by both backends so the two agree by construction.
func LiteralIdent(lit string) (string, bool) {
	var b []rune
	for _, r := range lit {
		if n, ok := punctName[r]; ok {
			b = append(b, []rune(n)...)
			continue
		}
		b = append(b, r)
	}
	name := exportName(string(b))
	if name == "" || name == "X" {
		return "", false
	}
	return name, true
}

// OperatorProse describes a class operator in words, one line per element, for
// the generated doc comment. Shared by both backends: the prose is a fact about
// the API and about this ABI, and writing it twice is how the two drift.
//
// It says which LUA EXPRESSION the binding is, because that is the one thing a
// mod author cannot look up -- the API documentation lists these under a class's
// "operators" heading and every real mod writes them as `inv[1]`, so a reader
// who greps the generated file for `index` finds nothing and concludes the
// class is a three-member stub. That conclusion is exactly what fklua-ports'
// resource-marker (RM1) spent a morning on.
func OperatorProse(class string, m Member, name string) []string {
	switch m.Kind {
	case MemberGlobalFunc:
		out := []string{
			name + " is Factorio's GLOBAL " + m.Name + "(). It is on no class, so",
			"it is a top-level binding here rather than a method, and the handle",
			"the dispatch import takes is ignored for it.",
		}
		// THE PROFILER SENTENCE, on `log` alone, because it is the reason this
		// kind exists and a caller who does not know it has no way to find it:
		// LuaProfiler exposes no accessor for its own duration, and this is the
		// only place the engine renders one.
		if m.Name == "log" {
			out = append(out,
				"",
				"IT TAKES A LocalisedString, WHICH IS WHAT MAKES IT THE ONLY WAY TO READ",
				"A LuaProfiler. LuaProfiler has no accessor returning its duration --",
				"the engine renders one only as an ELEMENT of a localised string, so",
				"log{\"\", \"took \", p} is the whole idiom and what lands in",
				"factorio-current.log is: ... Duration: 12.368959ms",
				"",
				"In tier-2 terms that is an array of OfString(\"\"), OfString(\"took \")",
				"and OfObject(p) -- an empty first element is LocalisedString's",
				"\"concatenate the rest\" form. For a plain string with no localisation",
				"and no profiler in it, fk.Log is one import rather than a host call.")
		}
		if m.Name == "localised_print" {
			out = append(out,
				"",
				"It writes to STDOUT rather than to the log file, which is what it is",
				"for: a tool that launched Factorio as a child process reads it there.",
				"A headless run's stdout is the terminal, so nothing in a log file",
				"records it.")
		}
		if m.Name == "table_size" {
			out = append(out,
				"",
				"NOT FOR A LuaCustomTable, which the description says outright: use the",
				"class's own length operator, which answers without the table ever",
				"crossing. This counts the keys of a plain Lua table, which for a guest",
				"means a tier-2 value it built or one a callback handed it.")
		}
		return out
	case MemberIndex:
		key := "the key"
		if len(m.Args) > 0 && m.Args[0].Kind == KindU32 {
			key = "a 1-BASED position"
		}
		return []string{
			name + " is this class's INDEX operator: the Lua expression " +
				lowerFirstWord(class) + "[k], with " + key + " as the argument.",
			"It is a member here because an operator has no name to resolve --",
			"the ABI dispatches on the kind. Reading one entry costs one host",
			"call, where the whole-dictionary attribute costs the whole table.",
		}
	case MemberIndexSet:
		key := "the key"
		if len(m.Args) > 0 && m.Args[0].Kind == KindU32 {
			key = "a 1-BASED position"
		}
		out := []string{
			name + " is this class's INDEX-ASSIGN operator: the Lua expression " +
				lowerFirstWord(class) + "[k] = v, with " + key + " and the value",
			"both arguments. The API description declares no write side for an",
			"index operator -- it carries a read_type and nothing else -- so this",
			"is emitted from an allowlist over what the description says in PROSE.",
		}
		// WHICH RECEIVERS ACCEPT IT is per receiver rather than per class, so
		// the doc has to say it: a caller holding the wrong custom table gets a
		// status and no other warning.
		if class == "LuaCustomTable" {
			out = append(out,
				"",
				"WRITABILITY IS THE TABLE'S, NOT THIS CLASS'S. The API says settings.global,",
				"settings.player_default, player.mod_settings, settings.get_player_settings()",
				"and style.column_alignments may be written -- by overwriting a whole",
				"ModSetting table, which here is a tier-2 map with one \"value\" key -- and",
				"every other custom table in the game is read-only. Writing one of those",
				"answers ERR_CALL_FAILED carrying the engine's own \"LuaCustomTable is read",
				"only\", and writing a key that is not a defined setting answers it with",
				"\"doesn't contain key\". This is the only way a mod changes its own",
				"runtime-global setting.")
		}
		if class == "LuaFluidBox" {
			out = append(out,
				"",
				"Writing an ABSENT value clears the fluid box, which is the description's own",
				"sentence: \"Writing nil removes all fluid from the fluid box.\" New fluid",
				"boxes may not be added or removed this way, and the index must be in bounds.")
		}
		return out
	case MemberLen:
		return []string{
			name + " is this class's LENGTH operator: the Lua expression #" +
				lowerFirstWord(class) + ".",
		}
	case MemberSelf:
		return []string{
			name + " is this class's CALL operator: the Lua expression " +
				lowerFirstWord(class) + "(...), calling the object itself.",
		}
	}
	return nil
}

// lowerFirstWord turns LuaChunkIterator into `it`-ish prose: the class name
// with its Lua prefix dropped and its first letter lowered, which is what a mod
// author's variable is usually called.
func lowerFirstWord(class string) string {
	s := class
	if len(s) > 3 && s[:3] == "Lua" {
		s = s[3:]
	}
	if s == "" {
		return "obj"
	}
	if s[0] >= 'A' && s[0] <= 'Z' {
		s = string(rune(s[0])+32) + s[1:]
	}
	return s
}

// buildOperator turns one class operator into a member entry.
//
// Two shapes, and the JSON tells them apart by which fields it carries:
// __index and __len are ATTRIBUTE-shaped (a read_type and nothing else, nine of
// the eleven) and __call is METHOD-shaped (parameters and return values).
//
// WHERE THE INDEX KEY'S TYPE COMES FROM, since the description does not carry
// one. An operator declares only what indexing YIELDS, so the key has to be
// derived, and the rule is two clauses over facts the description does state:
//
//   - A class that also declares `length` answers Lua's `#`, which is the
//     SEQUENCE-length operator, so it is indexed by position: uint32.
//     LuaFluidBox's own index description says so out loud -- "the index must
//     always be in bounds (see length_operator)" -- and LuaInventory's example
//     is `get_main_inventory()[1]`.
//   - ...unless what it yields is itself tier 2, which is the description
//     saying the class is heterogeneous. LuaCustomTable yields `Any`, and it
//     really is keyed by `uint32 | string` at half its use sites
//     (game.players) and by string at the other half (force.technologies). A
//     union key is exactly what a dyn-keyed dictionary return already crosses
//     as, so this is the answer the generator gives one line away in goDictKV
//     rather than a new one.
//
// That leaves LuaGuiElement, which declares no `length` and whose index is by
// child NAME, on the tier-2 arm as well -- correct for the same reason.
// TestOperatorKeyKinds enumerates all five so a pin that adds a sixth fails
// rather than being classified by a rule nobody re-read.
//
// THE WRITE HALF IS AN ALLOWLIST, and until 2026-08-24 there was none at all.
// An operator carries a read_type and never a write_type, so `fluidbox[1] = f`
// and `settings.global["x"] = {value = true}` -- both of which the description's
// own PROSE says are legal -- were not shapes any generator could reach. See
// indexWriteHalf, which is where that prose is read, and buildIndexSetter,
// which is the member it produces.
func buildOperator(m *typeMapper, class string, op Operator, hasLength bool) (Member, error) {
	if !op.IsAttribute() {
		// __call. Positional parameters and return values, exactly like a
		// method -- so it is built like one and only the kind differs.
		mem, err := buildMethod(m, class, Method{
			Name: op.Name, Order: op.Order,
			Parameters: op.Parameters, ReturnValues: op.ReturnValues,
			Format: op.Format,
		})
		if err != nil {
			return Member{}, err
		}
		mem.Kind = MemberSelf
		return mem, nil
	}

	val, err := m.mapType(*op.ReadType, 1)
	if err != nil {
		return Member{}, err
	}
	val.Name, val.Optional = "value", op.Optional

	switch op.Name {
	case "length":
		return Member{Class: class, Name: op.Name, Kind: MemberLen,
			Rets: []FieldSpec{val}, Optional: op.Optional}, nil
	case "index":
		return Member{Class: class, Name: op.Name, Kind: MemberIndex,
			Args: []FieldSpec{indexKey(val, hasLength)}, Rets: []FieldSpec{val},
			Optional: op.Optional}, nil
	}
	// A fourth attribute-shaped operator would be a Lua metamethod this ABI has
	// no expression for, and guessing which one is how a wrong binding ships.
	return Member{}, fmt.Errorf("attribute-shaped class operator %q, which is "+
		"neither __index nor __len", op.Name)
}

// indexKey derives the key FieldSpec from what indexing yields. Its two clauses
// are buildOperator's own and are written up there; it is a function so that
// the READ and the WRITE cannot derive the key differently -- a class indexed by
// position for a get and by a tier-2 value for a set would be two questions
// about one identity answered twice, which is the shape this repo has been
// bitten by often enough to stop writing.
func indexKey(val FieldSpec, hasLength bool) FieldSpec {
	key := FieldSpec{Name: "key", Kind: KindDyn}
	if hasLength && val.Kind != KindDyn {
		key.Kind = KindU32
	}
	return key
}

// buildIndexSetter turns one index operator into the `obj[k] = v` member, for
// the classes indexWriteHalf says have a write half.
//
// THE VALUE TYPE MIRRORS THE READ TYPE, INCLUDING ITS OPTIONALITY, and that is
// the whole derivation. The description gives an operator exactly one type; a
// write half that took a different one would be this generator inventing a
// signature. Optionality carries for a reason rather than for symmetry:
// LuaFluidBox's index is declared optional and its prose spends a sentence on
// what an absent value MEANS ("Writing `nil` removes all fluid from the fluid
// box"), so the presence byte is the clear gesture. M.call trims to the last
// argument present, so an absent value reaches the engine as `obj[k] = nil`
// with no special case anywhere.
//
// No return values: an assignment is not an expression in Lua, and a caller who
// wants to know what is there now asks the read operator.
func buildIndexSetter(m *typeMapper, class string, op Operator, hasLength bool) (Member, error) {
	val, err := m.mapType(*op.ReadType, 1)
	if err != nil {
		return Member{}, err
	}
	val.Name, val.Optional = "value", op.Optional
	return Member{Class: class, Name: op.Name, Kind: MemberIndexSet,
		Args: []FieldSpec{indexKey(val, hasLength), val}}, nil
}

func buildMethod(m *typeMapper, class string, meth Method) (Member, error) {
	if meth.VariadicParameter != nil {
		return Member{}, fmt.Errorf("variadic parameter")
	}
	out := Member{Class: class, Name: meth.Name, Kind: MemberCall}

	// A PARAMETER THAT CAN BE A LUA FUNCTION IS ONE NO GUEST CAN FILL, and the
	// member is marked rather than skipped. See Member.Unfillable: the host
	// binds it, the marshalling is right, and only the guest side has nothing to
	// send. A union of function and nil collapses to tier 2 here, which is a
	// correct encoding of a value that is only ever nil.
	for _, p := range meth.Parameters {
		if typeCanBeAFunction(p.Type) {
			out.Unfillable = UnfillableHandler
			break
		}
	}

	if len(meth.VariantGroups) > 0 {
		// The method's own parameter table is a discriminated union -- at the GA
		// pin the four are set_gui_arrow, LuaGuiElement::add, create_entity and
		// create_segmented_unit, and 2.1.17 adds a fifth
		// (LuaSimulation::get_widget_position), so the POPULATION is a
		// measurement per description rather than the constant this comment used
		// to state. Same answer as a variant-group concept: one tier-2 argument,
		// which the guest fills as a tagged table.
		out.Args = []FieldSpec{{Name: "args", Kind: KindDyn}}

		// ...AND A SECOND, TYPED ARGUMENT LIST OVER THE SAME MEMBER ID. The
		// tier-2 form above is what makes these members reachable at all; it is
		// also 3.3x the cost of a flat block (agents/drafts/r4b-batched-gui-add.md)
		// and 341 field names that appear nowhere in the guest's language. So the
		// SHARED parameters -- every parameter the description declares outside a
		// variant group -- lay out as an ordinary tier-1 struct, and the variant
		// tail crosses beside it as one optional tier-2 slot.
		//
		// TWO TOP-LEVEL FIELDS RATHER THAN AN `extra` FIELD INSIDE THE STRUCT,
		// and that is a correctness decision rather than a shape preference: a
		// field inside the block would occupy a KEY, and a variant group is free
		// to declare a parameter of any name. Measured -- create_entity's shared
		// `target` is ALSO a variant-group parameter, at every committed pin --
		// so the two namespaces really do overlap, and keeping the tail outside
		// the block means it can never collide with the block's own field names.
		//
		// A member whose shared parameters do not map keeps the tier-2 form
		// alone. Nothing is lost: the typed form is additive over an id that
		// already works.
		if fields, err := m.mapFields(meth.Parameters, nil, 1); err == nil && len(fields) > 0 {
			out.TypedArgs = []FieldSpec{
				{Name: "args", Kind: KindStruct, Struct: fields},
				{Name: "extra", Kind: KindDyn, Optional: true},
			}
		}
	} else if meth.TakesTable() {
		// One struct argument rather than positional ones, which is exactly the
		// tier-1 shape.
		fields, err := m.mapFields(meth.Parameters, nil, 1)
		if err != nil {
			return Member{}, err
		}
		if len(fields) > 0 {
			out.Args = []FieldSpec{{Name: "args", Kind: KindStruct, Struct: fields}}
		}
	} else {
		params := append([]Parameter(nil), meth.Parameters...)
		sort.SliceStable(params, func(i, j int) bool { return params[i].Order < params[j].Order })
		for _, p := range params {
			f, err := m.mapType(p.Type, 1)
			if err != nil {
				return Member{}, fmt.Errorf("parameter %q: %w", p.Name, err)
			}
			f.Name, f.Optional = p.Name, p.Optional
			out.Args = append(out.Args, f)
		}
	}

	rets := append([]ReturnValue(nil), meth.ReturnValues...)
	sort.SliceStable(rets, func(i, j int) bool { return rets[i].Order < rets[j].Order })
	for i, rv := range rets {
		f, err := m.mapType(rv.Type, 1)
		if err != nil {
			return Member{}, fmt.Errorf("return %d: %w", i, err)
		}
		f.Name, f.Optional = fmt.Sprintf("r%d", i), rv.Optional
		out.Rets = append(out.Rets, f)
	}
	return out, nil
}

// classify buckets a skip reason so the report counts causes rather than
// messages. What it says is which missing piece would buy the most coverage.
func classify(err error) string {
	s := err.Error()
	for _, probe := range []struct{ needle, bucket string }{
		{"tier-2 codec", "union or recursive type (tier 2)"},
		{"variant parameter groups", "variant parameter groups (hand-written)"},
		{"untyped table", "untyped table"},
		{"callback", "callback parameter"},
		{"tuple", "tuple"},
		{"LuaLazyLoadedValue", "LuaLazyLoadedValue"},
		{"variadic", "variadic parameter"},
		{"nil has no representation", "nil"},
		{"no expressible fields", "empty table"},
	} {
		if contains(s, probe.needle) {
			return probe.bucket
		}
	}
	return "other"
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Emitting the member table
//
// The generated Lua ships inside the mod, alongside the guest it was generated
// with. That pairing is what makes dense per-build member ids safe: the table
// and the wasm always come from the same API version, so they cannot disagree.
// ---------------------------------------------------------------------------

// blocks lays out a member's argument and return signatures.
//
// A field with no name gets a positional one. Parameter names come from the
// JSON and are almost always present, but the layout is keyed by name and a
// blank would collide with the next blank.
func (m Member) blocks() (StructBlock, StructBlock, error) {
	name := func(fs []FieldSpec, prefix string) []FieldSpec {
		out := append([]FieldSpec(nil), fs...)
		for i := range out {
			if out[i].Name == "" {
				out[i].Name = fmt.Sprintf("%s%d", prefix, i)
			}
		}
		return out
	}
	args, err := LayoutStruct(name(m.Args, "a"))
	if err != nil {
		return StructBlock{}, StructBlock{}, fmt.Errorf("%s::%s args: %w", m.Class, m.Name, err)
	}
	rets, err := LayoutStruct(name(m.Rets, "r"))
	if err != nil {
		return StructBlock{}, StructBlock{}, fmt.Errorf("%s::%s rets: %w", m.Class, m.Name, err)
	}
	return args, rets, nil
}

// typedBlock lays out the second, typed argument list. See Member.TypedArgs.
//
// It is the SAME LayoutStruct the ordinary argument block goes through, which is
// the whole reason a typed block costs no new marshalling machinery: presence
// bytes where Placed.HasOffset puts them, (ptr, len) for a string, natural
// alignment. The host reads it with read_struct.
func (m Member) typedBlock() (StructBlock, bool, error) {
	if len(m.TypedArgs) == 0 {
		return StructBlock{}, false, nil
	}
	blk, err := LayoutStruct(m.TypedArgs)
	if err != nil {
		return StructBlock{}, false, fmt.Errorf("%s::%s typed args: %w", m.Class, m.Name, err)
	}
	return blk, true, nil
}

// luaQuote renders a Lua string literal.
//
// \ddd escapes rather than Go's %q, for the same reason luagen has its own:
// Go emits \u for a non-ASCII rune and Lua 5.2 cannot parse that. Member names
// are ASCII identifiers today, but the API is not ours and a name that breaks
// the whole chunk is a bad way to find that out.
func luaQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			out = append(out, '\\', c)
		case c < 0x20 || c >= 0x7f:
			out = append(out, []byte(fmt.Sprintf("\\%03d", c))...)
		default:
			out = append(out, c)
		}
	}
	return string(append(out, '"'))
}

// LuaSource renders the member table as the module a packaged mod requires.
//
// ArgSize and RetSize travel with each entry because the GUEST needs them: it
// has to reserve a block of the right size before it can call, and computing
// that from the field list at runtime would be work it already did at compile
// time.
func (r Report) LuaSource(a *API) (string, error) {
	return r.LuaSourceWith(a, EventReport{Reasons: map[string]int{}})
}

// LuaSourceWith renders the members and the events together. They share a file
// because they share a lifetime: both are generated from one API version and
// ship beside the guest compiled against them.
func (r Report) LuaSourceWith(a *API, ev EventReport) (string, error) {
	var b []byte
	add := func(format string, args ...any) {
		b = append(b, []byte(fmt.Sprintf(format, args...))...)
	}

	add("-- Generated by fklua from runtime-api.json. Do not edit.\n")
	add("--\n")
	add("-- Factorio %s, API schema version %d.\n", a.ApplicationVersion, a.APIVersion)
	add("-- %d members; %d were not expressible and are absent rather than broken.\n",
		len(r.Members), len(r.Skipped))
	add("--\n")
	add("-- Member ids are indices into this table and are paired with the guest\n")
	add("-- that shipped beside it. They are NOT stable across API versions and do\n")
	add("-- not need to be: both halves are regenerated together.\n")
	add("return {\n")
	add("  api_version = %d,\n", a.APIVersion)
	add("  application_version = %s,\n", luaQuote(a.ApplicationVersion))
	evSrc, err := ev.luaEvents()
	if err != nil {
		return "", err
	}
	add("%s", evSrc)
	defSrc, err := r.Defines.luaDefines()
	if err != nil {
		return "", err
	}
	add("%s", defSrc)
	add("  members = {\n")

	for _, m := range r.Members {
		args, rets, err := m.blocks()
		if err != nil {
			return "", err
		}
		valid := ""
		if m.HasValid {
			valid = "valid=true,"
		}
		// opt= is emitted only when true, so every member the description does
		// not call optional keeps the bytes and the meaning it always had.
		opt := ""
		if m.Optional && m.Kind != MemberSet {
			opt = "opt=true,"
		}
		// targsize= and targs= are emitted only for a member that HAS a typed
		// argument list, which is a method whose parameter table is a
		// discriminated union -- five of 4,262 at the widest committed pin. Every
		// other member's row is byte for byte what it was.
		targs, hasTyped, err := m.typedBlock()
		if err != nil {
			return "", err
		}
		tsize, ttab := "", ""
		if hasTyped {
			tsize = fmt.Sprintf("targsize=%d,", targs.Size)
			ttab = fmt.Sprintf(",targs=%s", targs.LuaTable())
		}
		add("    [%d]={kind=%d,name=%s,class=%s,%s%s%sargsize=%d,retsize=%d,"+
			"sig={args=%s,rets=%s%s}},\n",
			m.ID, m.Kind, luaQuote(m.Name), luaQuote(m.Class), valid, opt, tsize,
			args.Size, rets.Size, args.LuaTable(), rets.LuaTable(), ttab)
	}

	add("  },\n")
	add("}\n")
	return string(b), nil
}

// MemberIndex maps "Class::name" plus kind onto an id, which is what a binding
// generator needs to emit constants and what a diff needs to compare versions.
func (r Report) MemberIndex() map[string]int {
	out := make(map[string]int, len(r.Members))
	for _, m := range r.Members {
		out[fmt.Sprintf("%s::%s/%d", m.Class, m.Name, m.Kind)] = m.ID
	}
	return out
}

// ---------------------------------------------------------------------------
// Pruning: shipping only the members a guest actually calls
//
// The full table is about a megabyte of Lua for the whole member set
// (host_members_bound in census.json; the exact byte count moves with every
// pin and is not worth a literal here). Putting that in every mod would make a
// guest that calls five API members carry thousands it never touches -- in
// every save, in every download, and in Factorio's parse time at load.
//
// IDS ARE PRESERVED, NEVER RENUMBERED. The guest baked them in when its
// bindings were generated, so the pruned table is SPARSE: `members[1729]`
// stays 1729. Renumbering to close the gaps would silently point every call at
// the wrong member, which is the worst possible way to save a few kilobytes.
// ---------------------------------------------------------------------------

// Only returns a report holding just the given member ids, with their ids and
// signatures unchanged. Skips are carried through untouched: what a build could
// not express is still worth reporting even when the guest never asked for it.
func (r Report) Only(ids map[int]bool) Report {
	// Defines rides along: it was pruned by its OWN scan, and dropping it here
	// would silently un-generate every define the caller had already resolved.
	out := Report{Skipped: r.Skipped, Reasons: r.Reasons, Defines: r.Defines}
	for _, m := range r.Members {
		if ids[m.ID] {
			out.Members = append(out.Members, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Events
//
// `events` in census.json counts them; they average around five fields and
// nearly all are expressible with tier 1 alone --
// events are flat, which is exactly why the plan encodes them EAGERLY. The
// alternative, a host call per field, would cost more than writing the whole
// struct for anything but a handler that reads one field and returns.
// ---------------------------------------------------------------------------

// EventDef is one generated event.
type EventDef struct {
	// ID is the index the guest subscribes with and receives.
	ID int
	// Name is the defines.events key. The NAME travels, not the number:
	// Factorio's define values are not stable across versions, so control.lua
	// resolves defines.events[name] at load rather than baking one in.
	Name   string
	Fields []FieldSpec
}

// ConfChangedConcept is the concept script.on_configuration_changed hands its
// handler, and the one HOOK PAYLOAD that is not an event.
//
// It is a described concept like any other -- three booleans-and-strings plus
// two dictionaries -- and nothing in the API references it, so no generator had
// ever emitted it and the hook dispatched with no arguments at all. A guest
// therefore could not read `mod_changes` (which neighbour appeared, disappeared
// or moved version, and from what), `mod_startup_settings_changed` or
// `migration_applied`. Four of the thirteen mods the temptations survey audited
// branch on mod_changes directly, and every consumer of the ecosystem's standard
// migration module does so transitively.
const ConfChangedConcept = "ConfigurationChangedData"

// EventReport is the generated event table plus what it could not express.
type EventReport struct {
	Events  []EventDef
	Skipped []Skip
	Reasons map[string]int
	// ConfChanged is ConfChangedConcept's field list, or nil when this
	// description does not carry the concept or this layer cannot express it.
	//
	// IN THE EVENT REPORT rather than in a table of its own, because it IS an
	// event payload in every way that matters here: the host encodes it with
	// H.write_struct into the same per-level scratch buffer, the guest decodes it
	// with a generated reader, and the layout is computed by the same
	// LayoutStruct. What differs is only that Factorio raises it through a hook
	// rather than through script.on_event, so it has no id and no filters.
	ConfChanged []FieldSpec
	// ConfChangedSkip is why there is no layout, for the census. Empty when
	// there is one.
	ConfChangedSkip string
	// Omitted mirrors Report.Omitted for event payload fields. No event carries
	// one today; the row exists so that a payload that grows one is a number in
	// the census rather than a field that quietly stops arriving.
	Omitted   []OmittedField
	OmittedBy map[string]int
	// MaxSize is the largest encoded event. control.lua allocates ONE scratch
	// buffer of this size and reuses it, so an event dispatch allocates nothing.
	MaxSize int
}

// GenerateEvents builds the event table.
func GenerateEvents(a *API) EventReport {
	m := newTypeMapper(a)
	r := EventReport{Reasons: map[string]int{}}

	evs := append([]Event(nil), a.Events...)
	sort.Slice(evs, func(i, j int) bool { return evs[i].Name < evs[j].Name })

	for _, e := range evs {
		// Name the payload, so an omitted field says which event it was in.
		m.owner = append(m.owner, "event "+e.Name)
		fields, err := m.mapFields(e.Data, nil, 1)
		m.owner = m.owner[:len(m.owner)-1]
		if err != nil {
			r.Skipped = append(r.Skipped, Skip{Class: "event", Name: e.Name, Reason: err.Error()})
			r.Reasons[classify(err)]++
			continue
		}
		blk, err := LayoutStruct(fields)
		if err != nil {
			r.Skipped = append(r.Skipped, Skip{Class: "event", Name: e.Name, Reason: err.Error()})
			r.Reasons["layout"]++
			continue
		}
		if blk.Size > r.MaxSize {
			r.MaxSize = blk.Size
		}
		r.Events = append(r.Events, EventDef{Name: e.Name, Fields: fields})
	}
	for i := range r.Events {
		r.Events[i].ID = i + 1
	}

	// THE HOOK PAYLOAD, through the same mapper and the same layout.
	//
	// Asked for by NAME rather than found by walking members, because nothing in
	// the API references this concept -- which is exactly why it had never
	// generated. A description that stops carrying it leaves ConfChanged nil and
	// the hook dispatches with no argument, which is what it always did.
	m.owner = append(m.owner, ConfChangedConcept)
	if f, err := m.mapType(Type{Name: ConfChangedConcept}, 0); err != nil {
		r.ConfChangedSkip = err.Error()
	} else if f.Kind != KindStruct {
		r.ConfChangedSkip = "not a struct"
	} else if _, err := LayoutStruct(f.Struct); err != nil {
		r.ConfChangedSkip = err.Error()
	} else {
		r.ConfChanged = f.Struct
	}
	m.owner = m.owner[:len(m.owner)-1]

	r.Omitted, r.OmittedBy = m.omissions()
	return r
}

// WithoutConfChanged drops the hook payload's layout.
//
// PRUNED BY THE GUEST'S EXPORT, which is the one pruning key in this file that
// is not a constant-id scan: there is no id to find, because the host raises the
// hook rather than the guest asking for it. A guest that does not export
// fk_on_configuration_changed can never be handed one, so the layout would be
// bytes in every save for a dispatch that cannot happen -- and a mod that
// exported nothing new must package exactly what it packaged before.
func (r EventReport) WithoutConfChanged() EventReport {
	// The SKIP reason is deliberately kept. It says the layer could not express
	// the concept, which is a fact about the description and the generator and
	// stays true whichever guest is being packaged; what is dropped is the
	// layout, which is a fact about this package.
	r.ConfChanged = nil
	return r
}

// Only keeps just the given event ids, ids unchanged. Same rule as members: the
// guest baked them in, so renumbering would subscribe it to the wrong events.
func (r EventReport) Only(ids map[int]bool) EventReport {
	out := EventReport{Skipped: r.Skipped, Reasons: r.Reasons, MaxSize: r.MaxSize,
		Omitted: r.Omitted, OmittedBy: r.OmittedBy,
		ConfChanged: r.ConfChanged, ConfChangedSkip: r.ConfChangedSkip}
	for _, e := range r.Events {
		if ids[e.ID] {
			out.Events = append(out.Events, e)
		}
	}
	return out
}

// luaEvents renders the event table.
func (r EventReport) luaEvents() (string, error) {
	var b []byte
	add := func(f string, a ...any) { b = append(b, []byte(fmt.Sprintf(f, a...))...) }

	// THE SCRATCH BUFFER HAS TO HOLD THE HOOK PAYLOAD TOO, and only when the
	// payload is actually packaged. control.lua allocates one buffer per nesting
	// level at this size and encodes the configuration-changed payload into it
	// like an event; folding the size in unconditionally would move
	// event_scratch for every mod in existence, including ones that do not
	// export the hook and can never be handed one.
	scratch := r.MaxSize
	var ccBlk StructBlock
	if r.ConfChanged != nil {
		blk, err := LayoutStruct(r.ConfChanged)
		if err != nil {
			return "", fmt.Errorf("%s: %w", ConfChangedConcept, err)
		}
		ccBlk = blk
		if blk.Size > scratch {
			scratch = blk.Size
		}
	}
	add("  event_scratch = %d,\n", scratch)
	if r.ConfChanged != nil {
		add("  confchanged = {size=%d,fields=%s},\n", ccBlk.Size, ccBlk.LuaTable())
	}
	add("  events = {\n")
	for _, e := range r.Events {
		blk, err := LayoutStruct(e.Fields)
		if err != nil {
			return "", fmt.Errorf("event %s: %w", e.Name, err)
		}
		add("    [%d]={name=%s,size=%d,fields=%s},\n",
			e.ID, luaQuote(e.Name), blk.Size, blk.LuaTable())
	}
	add("  },\n")
	return string(b), nil
}

// ---------------------------------------------------------------------------
// Defines
//
// Counted by `defines` and `define_values` in census.json, which is the only
// place those numbers are written down. They are
// tier 3 -- plain i32 -- and the ONLY hard thing about them is that
// runtime-api.json carries their NAMES and an order and NOT their values.
//
// That is not an oversight to work around: a define's number is Factorio's own
// and is not stable across versions, which is why this ABI has always resolved
// defines.events by name at load rather than baking one in. Generating a
// constant from the pin is therefore not merely fragile, it is impossible --
// there is nothing in the description to generate FROM. The downstream report
// that asked for baked constants had the premise wrong, and the correct shape
// is the one defines.events already uses, generalised: the table carries the
// dotted PATH, control.lua resolves it against the running game, and the guest
// holds a per-build id that indexes the result.
// ---------------------------------------------------------------------------

// DefineDef is one generated define value.
type DefineDef struct {
	// ID is the per-build index the guest asks for through fk.define. Dense
	// and 1-based: 0 is reserved so an unresolved read is distinguishable.
	ID int
	// Path is the dotted path under `defines`, e.g. "direction.east". The NAME
	// travels, never the number.
	Path string
}

// DefineReport is the generated defines table.
type DefineReport struct {
	Defines []DefineDef
}

// definePaths flattens the define groups to the dotted path of every VALUE,
// sorted.
//
// EVERY GROUP EXCEPT defines.events. That one already has a resolved table of
// its own, and its numbers are not what fk.subscribe takes -- offering a guest
// both spellings of "on_tick" would be a trap dressed as a convenience.
//
// Sorted, so the ids GenerateDefines assigns over this depend on the API
// description and nothing else. Determinism is a correctness property here for
// the same reason it is everywhere else in this package: two machines building
// the same mod must produce the same table.
//
// A FUNCTION RATHER THAN A WALK WRITTEN WHEREVER ONE IS NEEDED, because there
// are two callers with opposite failure modes and they must agree: this one
// decides which paths a guest can ASK for, and `diffDefines` decides which
// paths an upgrade TOOK AWAY. A copy that drifted by one rule -- the events
// skip, say -- would report a define as removed that no guest could have read,
// or say nothing about one every guest reading it lost.
func definePaths(a *API) []string {
	var paths []string
	var walk func(prefix string, d Define)
	walk = func(prefix string, d Define) {
		path := prefix + d.Name
		for _, v := range d.Values {
			paths = append(paths, path+"."+v.Name)
		}
		for _, s := range d.Subkeys {
			walk(path+".", s)
		}
	}
	for _, d := range a.Defines {
		if d.Name == "events" {
			continue
		}
		walk("", d)
	}
	sort.Strings(paths)
	return paths
}

// GenerateDefines walks the define groups and assigns per-build ids.
func GenerateDefines(a *API) DefineReport {
	r := DefineReport{}
	for i, p := range definePaths(a) {
		r.Defines = append(r.Defines, DefineDef{ID: i + 1, Path: p})
	}
	return r
}

// Only keeps just the given define ids, ids unchanged -- the same rule as
// members and events, and for the same reason: the guest baked them in.
func (r DefineReport) Only(ids map[int]bool) DefineReport {
	out := DefineReport{}
	for _, d := range r.Defines {
		if ids[d.ID] {
			out.Defines = append(out.Defines, d)
		}
	}
	return out
}

// luaDefines renders the defines table: id -> dotted path, resolved at load.
func (r DefineReport) luaDefines() (string, error) {
	var b []byte
	add := func(f string, a ...any) { b = append(b, []byte(fmt.Sprintf(f, a...))...) }
	add("  defines = {\n")
	for _, d := range r.Defines {
		add("    [%d]=%s,\n", d.ID, luaQuote(d.Path))
	}
	add("  },\n")
	return string(b), nil
}
