# The recipes-and-research library — design round

Status: DRAFT for review, 2026-08-31. Nothing here is implemented. This answers the open questions the 2026-08-30 research round collected (agents/library-ecosystem.md carries the verdicts it builds on), recommends where a recommendation is honest, and flags what only the maintainer can decide. The measured engine facts cited (M-numbers) are the 2.1.17 probe set from that round; the enablers it depends on — `env(4)` ModName, `env(5)` defines.prototypes, the settings-dump gate hash, the in-game settings→data round trip — all shipped 2026-08-30.

## Charter

A guest library that lets a mod declare **user-configurable crafting recipes and research** for the items and technologies it introduces: recipes, tree position (prerequisites), and research cost, each optionally driven by startup settings the library generates. Shape: **FDSL's verb set + a settings layer + a prerequisite-cycle validator**, which is a genuinely new thing rather than a port — FDSL has no settings layer, and nothing in the ecosystem validates cycles (M4: the engine's cycle diagnostic is eight words with no name, no path, no mod).

**Data-stage-only and fkdata-only, therefore pin-transparent.** No `fkapi` import under any feature; the control-stage half is deliberately absent (see Q13). This is the property that makes it the right FIRST library: it cannot be broken by a pin move and it sits outside the entire stamp/lock/census machinery.

## The rules the engine measured for us (design inputs, not choices)

- Every dangling name — prerequisite, unlock, ingredient, result, science pack — is a HARD LOAD FAILURE naming the consumer's mod, inside whoever else's overhaul pack is installed (M2, M3, M5, M5b, M5c).
- A prerequisite cycle fails with *"Cycle in technology tree detected."* and nothing else (M4). The library must own cycle detection; the engine's diagnostic is useless.
- Same-type setting-name collisions are SILENT last-writer-wins (M7); only cross-type collisions are loud (M8). Prefixing is the only defence.
- `unit.count = 0` is refused; `default_value` outside `allowed_values` is refused at the settings stage (M6b, M6c).
- Recipe `ingredients` are the long dict form; technology `unit.ingredients` are the short tuple form, and the dict form there is REFUSED (M12). Do not generalise one to the other.
- 32 of 275 base technologies are `research_trigger` techs with no `unit` at all (BBB's measured crash class): read the whole `unit` map and check its tag, never index into it.
- A localised-string parameter must be a STRING at the data stage; a number is a hard error (M13).

## The questions, answered

**Q1/Q2 — naming and namespacing: DERIVED, never passed.** The library derives its prefix from `fkdata.ModName()` (shipped for exactly this) and refuses to emit any setting or prototype whose name it did not prefix. No prefix parameter: a parameter can drift from the packaged mod, and M7 makes the drift silent. The BBB `Resolve`-shape applies: the type system should make an unprefixed name unrepresentable, not advised against.

**Q3 — the cycle validator: YES, and it is the flagship.** Before any `extend` or prerequisite rewrite lands, the library reads every technology's `prerequisites` (measured cheap: 479 edges in base), overlays its planned edits, and walks for cycles. A cycle is refused naming the full path — the diagnostic the engine does not give. Same pass catches the dangling-prerequisite case with the library's own message before the engine's `assignID` abort.

**Q4 — prove-present-before-naming as a TYPE: YES.** An ingredient/unlock/prerequisite reference is a resolved value the API cannot construct from a bare string without a probe (`Get` returning present). BBB's ladder (fallback chain terminating at a guaranteed staple, drop rather than guess) ships as an optional helper, not as the only path.

**Q5 — "you removed something others depend on": OUT OF V1.** V1 never deletes and never rewrites another mod's tech except to splice a prerequisite, which the validator already covers. A removal pre-check is a v2 feature with its own design.

**Q6/Q7 — declarative tree placement, and cost-and-position as ONE choice.** The primitive is `InsertBetween(before, after)`: set the new tech's prerequisites to {before}, rewrite `after`'s prerequisites replacing `before` with the new tech. Degradation when an endpoint is absent (another mod removed it): degrade to `After(before)` alone and log one line — never guess a substitute. The BBB rule is adopted and ENFORCED in the surface: cost is expressed as `CostOf(tech)` — copy a named technology's whole `unit` — so tree position and price come from one named point by default; a hand-rolled `unit` is the explicit escape hatch, not the default.

**Q8 — multi-level and infinite techs: MOSTLY FREE, one refusal.** `CostOf` copies `count_formula` and `max_level` verbatim when the source has them — a formula is a string and copying needs no evaluator. What v1 refuses: `CostOf` on a `research_trigger` tech (no `unit` to copy — M-measured; refused with a message naming the tech and the reason) and arithmetic ON a formula (scaling `2^(L-7)*1000` by a setting needs the MathExpression grammar; v2, if ever).

**Q9 — `research_trigger`: handled as above; never a source of cost, always safe to name as a prerequisite.**

**Q10 — a disabled generated tech: HIDDEN, NOT ABSENT — maintainer decision flagged.** Recommendation: emit with `enabled = false` and `hidden = true` when the setting turns it off, rather than not emitting. Absent is cleaner for the prototype checksum, but a tech researched in an existing save whose prototype vanishes is dropped from the save, and a settings flip is exactly the mid-save event this library invites. The engine already resets technologies on a startup-settings change (its own migration doc), which favours keeping the prototype stable and toggling its fields. **Decide: hidden (recommended) vs absent.**

**Q11/Q12 — localisation: three-tier policy.** (1) Generated techs/recipes get inline `localised_name`/`localised_description` built from the consumer's own strings — no locale files needed (M11), numbers stringified (M13). (2) The settings layer PREFERS bool/int/double settings, whose names localise inline-adjacent; a string-setting dropdown is the one shape with no inline mechanism for its VALUES, so the API marks dropdown-taking constructors as requiring consumer locale. (3) The library ships a host-testable `.cfg` checker (pure Go/Rust, no fkdata) that a consumer's own tests run against the generated option list — BBB's hand-rolled tripwire, made standard. The wiki-vs-BBB fallback-rendering conflict stays unresolved and does not block: either way the checker is worth having.

**Q13 — data-only, NO control half: YES.** The engine resets recipes and technologies when mods, prototypes or startup settings change (its own documentation), so a control-side re-apply buys nothing — and a control half would cost the pin-transparency that makes this the right first library. If a real consumer surfaces a control-side need, it ships as a SEPARATE package so the data half stays pin-free.

**Q14 — enumeration/indexes: LINEAR, ON DEMAND.** 660 recipes / 20k leaves measured affordable at load; no index in v1. An FDSL-style `find_by_ingredient` is a helper over `Keys` + `Get` with a documented cost, not a cached structure.

**Q15 — which stage: the consumer's choice, library stage-agnostic.** Creating your own content belongs in `fk_data`; patching another mod's belongs in `fk_data_updates`; the library works from either and says so. The validator runs against whatever `data.raw` holds at the moment it is invoked.

**Q16 — parity: BOTH LANGUAGES FROM DAY ONE**, per docs/library-parity.md: one example guest per language exercising every verb, transcripts compared through the strict stand-in, and the settings/locale checker halves pinned by a committed golden.

**Q17 — gate hygiene: DONE** (shipped 2026-08-30; the settings-dump hash and the in-game round trip were prerequisites and no longer block).

## Surface sketch (Go; Rust mirrors)

```go
lib := fkrecipes.New()                       // prefix = fkdata.ModName() + "-"
enabled := lib.BoolSetting("enabled", true)  // name auto-prefixed, order stable

it   := lib.Item("widget", fkrecipes.Icon("__mymod__/graphics/widget.png"), ...)
rec  := lib.Recipe(it, fkrecipes.Ingredients(...), fkrecipes.CraftTime(2))
tech := lib.Technology("widgetry",
        fkrecipes.CostOf("logistics-2"),          // cost AND position from one tech
        fkrecipes.InsertBetween("logistics-2", "logistics-3"),
        fkrecipes.Unlocks(rec),
        fkrecipes.EnabledBy(enabled))             // off -> enabled=false + hidden

lib.Emit()   // validates (presence, cycles, unit shape), then extends; any
             // failure raises at the stage naming the path, per fkdata's rule
```

Everything before `Emit` is a PLAN in ordinary values — host-testable with no fkdata, which is where the validator's own tests live. `Emit` is the only fkdata-touching call. Plans are slices in declaration order; no map iteration anywhere (the data-stage determinism rule).

## Maintainer decisions needed before implementation

1. **Name**: `fkrecipes` (used above), `fktech`, or other.
2. **Where it lives**: in-repo beside fklog/fkipc (gets the repo's gates; recommended for v1) vs the first external library (dogfoods distribution but forfeits the gates until the harness recipe is proven out-of-tree).
3. **Q10**: hidden (recommended) vs absent for a disabled generated tech.
4. **V1 scope cut**: the sketch covers items, recipes, technologies. Trimming items (consumer brings their own item prototypes; library does recipes+techs only) roughly halves v1. Recommendation: keep items — the inline-locale story is most valuable there — but it is a legitimate cut.

## What v1 explicitly does not do

Delete or hide OTHER mods' content; evaluate or scale `count_formula`; touch the control stage; cache indexes; take a prefix parameter; emit anything unprefixed; iterate a map to decide anything.
