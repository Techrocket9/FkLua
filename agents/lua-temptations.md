# Lua temptations: what still pushes a mod author back to hand-written Lua

A closing survey of the gaps that could force ("BLOCKS") or tempt ("TEMPTS") a future FkLua consumer to write Lua by hand, against the project's stated target of replacing hand-written Lua in Factorio modding entirely. It records each gap with its evidence, the affected genre, a proposed shape in this project's own vocabulary, a cost estimate, and a priority ordering, plus the doctrine for the entry points that are legitimately out of scope. Everything here was gathered after the settings/data stage, data-only packaging, global functions, `fk.LastError`, `--dependency` and the test-observer idiom set closed; none of those is re-reported.

Method, in four passes: a census-by-genre pass over the 2.1.16 description, the generators and a real generated binding; an entry-point pass over every Lua entry point that is not the control, settings or data stage, with one headless probe of the migration environment; an audit of thirteen real, popular, genre-diverse mods (about 172,000 lines of Lua, every API member checked mechanically against the bindings); and a sweep of the two porting campaigns' written friction record (eight shipped mods plus a sixteen-observer test estate). Where two passes reached the same verdict independently, that is said, because it is the strongest evidence this survey has.

## The one-paragraph answer

Binding coverage is essentially total: 4,857 of 4,859 members bind at the 2.1.16 pin, the two deferrals are name collisions that cost nothing (the method half of each pair wins), and not one of the thirteen audited mods is blocked by a missing member. What blocks authors is narrower and sharper: one genre cannot receive its input at all (custom-input events, which nine of the thirteen mods use), one member binds green and can never work (`on_nth_tick`), one writable attribute lost its string arm in generation (`LuaGuiElement.style`), and the version-check gate is blind to a fifth of what a guest can read (defines). What tempts authors is ergonomic rather than structural: a tier-2 `Value` with seven constructors and zero accessors, the four variant-defeated members that happen to be the two most-used construction calls in modding, a per-call boundary with no bulk form, and behavioral rules filed where a new author will not look.

## Verdict table

| Gap | Verdict | Genre it hits | Shape | Cost |
|---|---|---|---|---|
| Custom-input event subscription | **BLOCKS** | keybinds, GUI apps, selection tools (9 of 13 mods) | a name parameter widening `fk.subscribe` | S |
| Mod-defined event subscription | **BLOCKS** | cross-mod integrations (LTN-style ecosystems) | third `fk.register` kind | M |
| `LuaGuiElement.style = "name"` | **BLOCKS** | any GUI that restyles at runtime | **FIXED (2026-08-30)**: an attribute write whose union would collapse to one handle arm crosses as tier 2 | S |
| `on_nth_tick` binds but is unfillable | **BLOCKS, silently** | polling mods | **the five handler members are DEFERRED (2026-08-30)**; first-class hook later | S then M |
| `fk_on_configuration_changed` discards its payload | **BLOCKS an idiom** | per-neighbour compatibility | pass `ConfigurationChangedData` as tier-2 | S-M |
| `api check` blind to defines | **BLOCKS the gate's promise** | every guest reading a define | wire `UsedDefines` in; leaf-aware define diff | S |
| GUI at application scale | TEMPTS, hard | GUI applications | `Value` accessors, typed `add`, batched add | S / M / M-L |
| `create_entity` untyped args | TEMPTS, hard | every entity-creating mod | typed args struct plus `Extra` | M |
| Per-call boundary, no bulk read | TEMPTS, hard | tick-per-entity mods | bulk attribute read | L |
| Behavioral rules filed as maintainer notes | TEMPTS, hard | every new author | relocate into `docs/` | S |
| Line builder duplicated per guest | TEMPTS | every guest | ship `fklog` in both languages | S |
| No way to inspect a `Value` | TEMPTS | every guest | `Value.Dump` plus a debugging doc | S |
| Optional-pointer fields force globals | TEMPTS | every guest | builder methods on args structs | M |
| Custom-table methods lack handle twins | TEMPTS | prototype browsers | lift twin emission into method returns | S |
| `remote.interfaces` has no point query | TEMPTS | overhauls' guard idiom | point-query member or seam helper | S |
| Data-stage emit vs the Lua function-length ceiling | TEMPTS | overhaul-scale data stages | split over-long functions, or a named check | M |
| `migrations/*.lua` | TEMPTS; defer the feature | mods with historical migrations | doctrine now, hook only on demand | S doc |
| Scenario packaging | small gap | mod-shipped scenarios | `[scenarios.<name>]` manifest key | S |
| Simulations, instrument mode, `load()`, Lua libraries, console Lua | OUT OF SCOPE | | doctrine, stated below | S doc |

## The blocks

### Custom-input events: the trap is baited

A keybind mod defines a `custom-input` prototype at the data stage (expressible in Go today) and subscribes to it by prototype name: `script.on_event("my-input", f)`. `LuaEventType` is a union of four arms and `fk.subscribe` reaches exactly one of them, the described `defines.events` set, through a dense integer index. The trap is that the description carries `CustomInputEvent` as an ordinary event, so the generator emitted a complete binding: the id constant, the payload struct, the reader, three field masks. A guest that subscribes to it compiles, passes the pruning scan, and at load the host resolves `defines.events[ev.name]`, gets nil because custom inputs are name-addressed, and logs a sentence claiming this Factorio has no such event. The one guest that found the right constant is told a falsehood and the hotkey never fires.

Nine of the thirteen audited mods subscribe to custom inputs by name (one of them, a hotkey-only mod, has no other entry point at all: every one of its seven subscriptions is a custom input, so the whole mod is unwritable). GUI applications lose every keyboard entry point. Selection-tool mods degrade to toolbar-only, which is survivable because `on_lua_shortcut` is an ordinary described event and binds.

The proposed shape is a name parameter on `fk.subscribe` itself, not a third `fk.register` kind. The distinction is load-bearing: `UsedEvents` prunes the packaged event table by scanning for an integer constant at the subscribe call's event operand, and a register descriptor is a tier-2 blob that scan cannot read, so the register shape would prune the payload descriptor out of the very mod that needs it. With the event id staying a constant in its own operand and the name arriving as a widening parameter (`SubscribeNamed(EventCustomInputEvent, "my-input")` on the guest side), the pruning scan, the payload machinery and the field masks are all untouched; the host uses the name as the registration key when one is present. Several custom inputs can share one registration and disambiguate on the payload's own `input_name`. Cost S: a few lines of host shim, one import parameter, one wrapper per guest language, no generator change. Whatever else is taken, the false log sentence should be corrected: for a name-addressed event the current text misdiagnoses the author's one mistake.

### Mod-defined events: the subscribe half of a published protocol

`generate_event_name`, `raise_event`, `get_event_id` and the custom-event prototype's `event_id` all bind, so an FkLua mod can be a publisher. It cannot be a consumer: a runtime-generated id is a number where `fk.subscribe` wants a table index, and a mod-defined event's payload is not in the description, so there is no field descriptor to encode with. The audit found whole ecosystems built on this shape: one train-logistics hub publishes eleven generated events as its entire public API, and none of its companion mods could be written on FkLua. This gap is recorded in no document today.

The shape here is the third `fk.register` kind after commands and remote interfaces: a descriptor naming a runtime event id (or a custom-event prototype name) and a guest export id, with the host synthesising the closure and the payload crossing as one tier-2 dynamic value, exactly as `fk_on_call` already does for remote calls, because an undescribed payload has no other honest encoding. Cost M. The two event fixes are complementary rather than competing: the subscribe widening serves described payloads cheaply and keeps pruning intact; the seam kind serves undescribed payloads, where there is nothing to prune.

### `LuaGuiElement.style = "name"`: a two-member defect class

`style` is a union of `LuaStyle` and `string` on the write side, and the engine accepts only the string there. Both generators collapsed the write to the handle arm, so the generated setter takes an `Object` and no string can be expressed. A description-wide scan finds exactly two writable union-with-class attributes; the other, `LuaControl.opened`, generated correctly as a `Value` setter. Four of the audited mods restyle at runtime (one at 31 sites). The mitigation, which is why this hid, is that `style` can be set at creation time inside the `add` table. Cost S: generate the write side of such unions as `Value`, matching `opened`, and regenerate.

### `on_nth_tick`: a census category that does not exist

The five handler-taking members of `LuaBootstrap` (`on_init`, `on_load`, `on_event`, `on_configuration_changed`, `on_nth_tick`) take a union of function and nil, which the union canonicaliser cannot type, so all five bind as a `handler Value`. Bound, required, and unfillable: no guest can construct a Lua function, and the only expressible argument is nil, which is Factorio's unregister. Four of the five are harmlessly shadowed by FkLua's own hooks. `on_nth_tick` is not: it presents as a green, plausible member whose every possible call is a silent no-op, and no census row, compile check or document says so. Seven of thirteen audited mods use it; the documented substitute (a self-re-arming `fk.defer` chain) works and costs up to one dispatch per tick where the engine's own form costs one per N.

Two shapes, in sequence. Now, at S: defer the five with a new census reason ("handler is a Lua function"), which costs nothing anyone can reach and converts the silent no-op into a compile error. Later, at M: a first-class periodic hook (`fk.on_nth_tick(n)` import with an `fk_on_nth_tick` export, armed state in `storage` the way the deferred flush already is), which removes the dispatch multiplier for the polling genre. A mod that arms and disarms timers dynamically (one audited mod does, at nine sites) is the case the first-class form serves and the defer chain multiplexes awkwardly.

### `fk_on_configuration_changed` discards `ConfigurationChangedData`

The hook dispatches with no arguments, so a guest cannot read `mod_changes` (which neighbour appeared, disappeared or moved version, and from what), `mod_startup_settings_changed` or `migration_applied`. Four audited mods branch on `mod_changes` directly and every consumer of the standard library's migration module does so transitively; the first FkLua consumer avoided it only because its own migration keys on a marker prototype rather than a version. The payload is a described concept, so the encode machinery exists. Cost S-M: marshal the payload into the existing hook (or a sibling that takes it), preserving the no-argument form for guests that export it.

### `api check` is blind to defines

The check's surface is members, events and the concepts reachable from their signatures. A guest's define reads (`fk.define`, one dense id per dotted path) are collected nowhere, so a guest reading a define that the target version removes gets a clean verdict; worse, `Complete` is computed from members and events alone, so a defines-only guest reports a complete scan having seen nothing. The diff underneath could not answer even if asked: it compares only top-level define group names, never values or subkeys, and measured between 2.0.77 and 2.1.16 it reports one breaking define (a group with no leaves that no guest can read) while twenty leaf paths were removed. The runtime failure is soft, one log line and a zero value forever, which is exactly the class of silent wrongness the check exists to catch. And because 1,108 of 1,117 surviving paths change id between those pins, the cross-reference must key on dotted paths resolved through the from-version's define report, never on ids.

The wiring: a leaf-aware define diff sharing the same prefix walk the accessor generator uses (so the diff and the pruner agree by construction); `GuestSurface` gains the define paths and a third completeness term; the check gains a define match kind and the verdict document a define count in its pinned key set. A ride-along found in the same file: the surface's event loop never collects concept names from event payload fields, where the member loop does. No payload concept happens to move between the committed pins, but the omission is structural rather than latent: at the GA pin five concepts are reachable only through an event payload, with no callable member ever surfacing them, so a guest depending on one had no cross-reference at all. Cost S in total.

## The temptations, ranked

### GUI: expressible in full, painful at application scale

The whole GUI surface binds: 198 of 198 members, all fifteen `on_gui_*` events with payload structs, styles, tags, and retained element handles that survive a save. Nothing structural stops a GUI mod, and per-click GUI work costs one dispatch and a handful of calls, which is fine. What remains is four facts stacking into the strongest temptation in the survey, confirmed independently by the census pass, the friction record and the real-mods audit:

- `add` is one of exactly four members whose option table defeats the typed-args generator (22 variant groups, 341 possible keys), so every element is a hand-built nested pair list. The other three include `create_entity`; between them the variant-defeated four are the most-called construction members in modding.
- `Value` has seven constructors and zero accessors, so every read of a tier-2 map is a hand-written linear scan and tag switch.
- The generator emits none of the description's member prose, and the docs renderer shows no parameter lists, so the 341 field names appear nowhere in the guest's language.
- Element count equals call count: `add` takes no children, so the audited GUI applications sit at 500 to 1,200 element-creation sites, and at the measured 12.5 microseconds per host call a 50-row table refresh costs 9-16 ms, up to a whole engine tick, against 1-2 ms in Lua. Whole-window rebuild is the dominant real-world pattern, and one audited overhaul updates a window per tick.

The fix stack, in value order: `Value` accessors (`Get(key)`, `Str`, `Num`, `Obj`, `Len`) at S, the highest value-per-line item in this survey, improving every one of the 598 `Value`-typed functions at once; typed args structs for the four variant-defeated members, generated from the shared parameters plus an `Extra` pair-list escape hatch for the variant tail, at M; description prose emitted as doc comments and variant-group fields rendered in `fklua docs`, at S-M; and a host-side batched add (an array of element specs applied in one crossing, returning the handles) at M-L, which is the only lever against the refresh cost. A worked GUI example belongs beside all of it, because today a GUI author finds zero prior art.

### The per-call boundary, and the genre it prices out

About 12.5 microseconds per host call, measured three independent ways, with no bulk form for per-entity attribute reads. A Lua loop over entities is nearly free; the same loop across the boundary is N calls. One audited resource monitor polls 100 to 1,000 entities per tick by design, which lands at 1.25 to 12.5 ms per tick on FkLua against about 0.05 in Lua: not blocked by any API gap, blocked by arithmetic. The friction record calls this the one cost no shipped fix removes. The shape is a bulk attribute read (attribute k for N handles in one crossing, into a destination slice, for which a precedent already exists in the filtered-search binding). Cost L, and it decides whether the tick-per-entity genre is reachable at all.

### The rules a new author needs are filed where maintainers look

The behavioral contract (peer-locality of the load hooks, constant-id pruning, iteration determinism, the atomic tick, allocation discipline) lives in the maintainer notes, while `docs/` documents artifacts. An author does not defect because Go is verbose; they defect after a desync whose rule was written down, measured, and filed under working notes. Relocating the contract into `docs/` (a rules page, and the player-experience tables beside the memory doc) is S and retires the most expensive late surprise the record shows. In the same round: a debugging page naming the log, `fk.Print`, `fk.LastError` and the value dumper below, because the recorded debugging loop today is recompile, repackage, rerun, diff a transcript.

### The per-guest boilerplate that should be a library

Every guest in both languages hand-rolls the same zero-allocation line builder (at least nine independent copies, one of which has already grown a real rounding divergence), because `fmt` links reflection and `format!` is large, and under a leaking collector both allocate permanently. Shipping `fklog` in both guest trees, with a `Value.Dump` writing into the same borrowed buffer, is S; the precedent for opt-in guest-side libraries is established. Adjacent S items from the same record: typed accessors on `ModSetting` (a bool setting currently arrives as a tagged union to switch on); a doc stating that map-seeded, save-persisted, desync-safe randomness already exists (`create_random_generator` binds; one shipped port replaced randomness with a modulo on the incorrect belief that no synced source existed, and the correct answer is currently written only inside generated output); a doc naming the log-and-gate idiom that replaces `error()`'s unwind; and the Rust polish batch (POD structs as `Copy`, parameter names carried into signatures, the missing `powf` note).

### Optional fields, globals, and the root set

A quarter of generated struct fields are pointers because they are optional; taking a local's address defeats TinyGo's stack allocation, so guests hoist buffers to package level; package globals are exactly what the collector re-scans at every mark, and the campaigns measured the cliff where two added globals took a post-load collection from 71 steps to 982. Builder methods on args structs (returning by value, so optionals need no addressable local) are M and pay three times: ergonomics, allocation, and the root-set pathology.

### Smaller structural temptations

Eleven methods returning a `LuaCustomTable` (the ten filtered prototype getters and per-player settings) lack the raw-handle and into-slice twins their attribute counterparts have, so each call materialises the whole table; lifting twin emission into method returns is S. `remote.interfaces` has no point query, so the standard existence guard copies every interface name in the save per check (one audited overhaul guards seventeen call sites); a point-query form is S. And an overhaul-scale data stage (the largest audited: 44,000 lines, ten times the first consumer's) turns the Lua function-length ceiling from a recorded footnote into a per-section discipline, with an engine message that names neither cause nor file; an emitter that splits over-long functions, or a packaging check that names the offender, is M and is the strongest single reason an overhaul author would leave a data stage in Lua.

## Migrations: doctrine now, mechanism on demand

Measured rather than assumed, with a two-phase probe: Lua migrations run in the mod's own control-stage Lua state, with `storage` fully restored, after `on_init` for newly added mods and before `on_load` and every mod's `on_configuration_changed`; the engine tracks them once per save by filename; a joining multiplayer client never runs them; and a mod newly added to a save has all its migrations run on the install load. The base game has shipped exactly one Lua migration in its whole 1.1 to 2.0 history; across ten sampled mods the median migration file is eleven lines, none needed the two properties only the mechanism has (running before every mod's hooks, engine-tracked once-per-save), and the ecosystem's standard library already routes migration work through `on_configuration_changed`.

The doctrine: a mod's own state is the guest's heap, and the heap is migrated by `fk_migrate`, never by a file the engine runs before the heap has been adopted. `migrations/*.lua` is not FkLua's state-migration mechanism and will not become one; the file type stays available through the include tree as a hand-written escape hatch with the status of inline assembly (permitted, marked, minimised, never generated), and the packager should print one line when an include tree carries one, so the count of hand-written Lua stays auditable. JSON migrations are data, not Lua, and are not a gap. Two caveats belong in the guest notes regardless of whether a migration hook is ever built: the runtime adopts the saved heap at `on_load`, one step after migrations run, so anything dispatched into a guest from a migration would run on a fresh heap that the adoption then overwrites; and `fk_migrate` triggers on the build stamp where Factorio's migrations trigger on the mod version, so a version bump that changes no wasm fires nothing, which is a different predicate than a Lua author expects.

## Out of scope, with the doctrine stated

- **Simulations** (tips, Factoriopedia, main menu): an `init` string is executed as a console command, so it can never load a compiled module; that is a property of the entry point, not a gap in the compiler. The base game and DLC carry about 352 KB of such inline Lua; mod uptake is thin. FkLua owes this entry point the one-line bridge, so the Lua a mod hand-writes here is a call and not a program: a simulation that lists the mod in `SimulationDefinition.mods` and calls into its remote seam keeps the whole screenplay in the guest. That recipe is assembled from documented pieces and has not been probed end to end; verify before publishing it.
- **Scenarios**: a scenario's `control.lua` is a full control stage, and the base game's own convention is a one-line require shim into the mod's tree, which is exactly the shape the packager already generates for the mod root. This is a packaging gap (a manifest key placing a generated shim under `scenarios/<name>/`), cost S, and low priority: a third of one percent of the portal ships scenarios, and the flagship scenario projects have migrated to mod form.
- **Instrument mode**: it injects Lua into other mods' states and disables multiplayer; an FkLua guest's program is not in its Lua state, so a debugger for it is a Go or Rust debugger. One sentence in the docs.
- **`load()` and published-Lua plugin protocols**: a cross-mod contract may be a protocol, not a program. The remote seam in both directions is the sanctioned surface; a mod whose extension point is Lua source cannot host an FkLua guest, and one shipped port dropped exactly such a mechanism. Say the loss out loud, and recommend data-payload protocols (the `mod_data` reads bind) to anyone designing one. The audited case of user-typed Lua (a formula bar, a Lua-syntax import format) is a bounded parser reimplementation, with the import case costing interoperability with strings users already share.
- **Cross-mod Lua libraries**: each mod's state copies the library source in, exactly as a Go import copies a package, so there is nothing to interoperate with; the answer to "how do I use flib from Go" is a Go package. What FkLua owes is the mapping table (each standard-library module to its guest-side equivalent: the migration module to `fk_on_configuration_changed`, the tick scheduler to the deferred flush, the geometry and table modules to the language itself), plus the honest residual that helper modules building prototype fragments do not flow through `data.raw` and so sit outside the data ABI's clone-and-patch model.
- **User-side console Lua**: out of any compiler's scope by construction; the mod side (commands, remote, RCON printing) binds.

## Where a reader would expect a gap and there is none

Recorded because absence of a gap is a result too: combat and unit AI (compound command structures with embedded handles compose cleanly into the bound command members; the largest audited AI mod is portable, and its 22,000-line world model is pure computation that improves in Go); trains, schedules, statistics (all sixteen flow-statistics members); blueprints, cursor and clipboard (bound with typed args; the mild residue is that a blueprint entity is a wide concept behind an untyped map); rendering (12 of 12 draw members, 115 render-object functions), sounds, rich text; per-player settings write (through the allowlisted custom-table write); RCON; selection tools and shortcuts (described events, full payloads; only the hotkey that hands you the tool is blocked, and a data-stage spawn-item input is a partial escape); serialization (the JSON and encode helpers bind); desync-safe randomness (bound, underdocumented); metatables (unnecessary for a guest rather than blocked); handle retention across saves (works and is the normal path in the later ports; the first consumer's re-find-everything rule is its own architectural choice, worth a doc note because that consumer is the exemplar newcomers read).

## Priority ordering

1. **The event-delivery round** (unblocks genres, mostly S): the subscribe name widening plus the corrected log sentence; defer the five unfillable handler members under a new census reason; the `style` union write fix; the configuration-changed payload; the defines wiring for `api check`.
2. **The GUI-and-hands round** (retires the widest temptations): `Value` accessors; typed args for the four variant-defeated members; description prose into doc comments and docs rendering; `fklog` and `Value.Dump`; the docs relocation (rules, debugging, randomness, the retention note, the flib mapping table).
3. **The seam round** (M items with designs in hand): the third register kind for runtime event ids; the first-class periodic hook; the custom-table method twins and the remote point query; the scenario shim key; the migrations audit line and the guest-notes rows (adopt ordering, build stamp versus version).
4. **The long levers** (L, schedule on demand): the bulk attribute read, which decides the polling genre; the batched GUI add, which decides the GUI-application genre at scale; the data-stage function splitter, which decides the overhaul genre's data half.

The census cannot see most of what this survey found: a member that binds and cannot fire, a hook that discards its payload, a check that is blind to a whole id space, and every ergonomic temptation are all invisible to a bound-versus-deferred count. Where a new capability lands, the census should grow the row that would have caught its absence, which is this project's own standing rule about zeros nobody writes down.

## The queue (2026-08-26)

The four rounds above are QUEUED as executable work; none is started. Amendments
since the survey was written, folded into their rounds:

- **Round 1** drops "the defines wiring for `api check`": it shipped with the
  survey itself (leaf-path keying and event-payload concept collection, both
  red-proven, merged). It gains the rename table for the two standing
  name-collision deferrals (takes both languages' deferrals to zero and turns
  the emission-ordering accident into a decision), and the packaging half of
  the one open downstream ledger item: `fklua mod` packages a stale wasm
  against fresh bindings without complaint at the same pin
  (BetterBeltBalancer's FKLUA-GAPS item 18); the lock hash is in hand at
  package time, so a mismatch should be at least a loud warning.
- **Round 2** gains the typed `ModSetting` accessors (Bool/Number/String over
  the untyped Value field).
- **Round 3** gains the verify-then-publish of the simulation bridge recipe
  (`SimulationDefinition.mods` plus a remote call into the seam; assembled
  from documented pieces, never probed end to end), and the diff blind spot
  the 2.1.17 bump measured: `api diff` walks a method's top-level parameters
  and never its variant groups, so a method gaining its first group or losing
  its last would flip its binding shape between dyn and positional silently.
  Zero instances existed in any shipped pair; the detector belongs beside the
  takes-table flip the diff already reports.
- **Round 4** gains Q5, the build-time configuration channel (a config written
  once reaching both a build tag and a startup setting), which the ports
  campaign left open.

Execution notes for whoever picks a round up: each round is a worktree off
master with the house gates (build the lua52f oracle first or thirty tests
skip as passes; `make test`; `gen-bindings --check` across every description;
red proofs per mechanism; docs in the same commit). Rounds are independent and
ordered by value rather than dependency, except that `fklog` precedes
`Value.Dump` inside round 2. If a round regenerates bindings, member ids can
move: the downstream stale-pair rule applies, and the round's report must say
whether consumers need a re-pin. The evidence behind every item is in this
file's body and the downstream audit records it cites.
