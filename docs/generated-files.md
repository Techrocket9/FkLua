# The generated files

FkLua writes files at three points in a project's life: `fklua init` scaffolds the project, `fklua gen-bindings` writes the Factorio API bindings, and `fklua mod` packages the mod. This page says what each file is, whether it is yours to edit, and how to regenerate the ones that are not.

| File | Written by | Yours to edit? |
|---|---|---|
| `fklua.toml` | `fklua init`, once | yes; it is the manifest |
| `guest/go/go.mod`, `gc.go`, `main.go` | `fklua init` | yes (keep `gc.go`'s import; see below) |
| `guest/rust/Cargo.toml`, `guest/rust/<name>/` | `fklua init` | yes |
| `guest/go/fkapi/` | `fklua gen-bindings` | no; regenerate |
| `guest/rust/fkapi/` | `fklua gen-bindings` | no; regenerate |
| `fklua.lock` | `fklua lock` | no; rerun `fklua lock` |
| `<name>_<version>/` or `.zip` | `fklua mod` | no; disposable output, repackage |

`init` refuses to overwrite anything it once wrote, per file, so a re-run after deleting one file restores that file and nothing else.

## fklua.toml: the manifest

The one file that describes the project. `fklua mod` and `fklua compile` read it, so packaging needs no flags; any flag given on the command line overrides the manifest.

`[mod]` becomes the packaged `info.json`:

| Key | What it is |
|---|---|
| `name`, `version`, `title`, `author`, `description` | the mod's identity, in Factorio's terms |
| `factorio_version` | the engine series the mod declares. Defaults to the API pin's `major.minor`; override it when the pin and the engine you target differ |
| `data` | a directory copied into the packaged mod: `data.lua`, `prototypes/`, `graphics/`, `locale/`. The default for `fklua mod --include` |
| `dependencies` | passed to `info.json` verbatim, in Factorio's own syntax: `"base >= 2.0.0"` required, `"? other"` optional, `"! other"` conflicts. `fklua mod --dependency DEP` overrides it |

`[fklua]` configures the toolchain:

| Key | What it is |
|---|---|
| `api` | the API pin: which committed `runtime-api.json` the bindings and the packaged member table come from. See [the version axes](factorio-api.md) |
| `lang` | the guest language(s) to generate bindings for: `["go"]`, `["rust"]`, or both |
| `gc` | how the guest heap is managed: `"collected"` (what `init` writes) or `"leaking"`. This key must agree with how the guest was built; a mismatch is a refusal at package time that names both sides. An absent key means "nobody said", which keeps pre-existing projects byte-for-byte unchanged. See [Memory, the collector and the save](memory.md) |

`[scenarios]` names the scenarios the mod ships, one key per scenario directory. It is optional, and a project without it packages exactly what it packaged before.

`[tool]` and every `[tool.<name>]` section are reserved for external tools, so an editor extension or a build wrapper can keep its own configuration in the manifest instead of a second file beside it. fklua ignores those sections entirely: every line between such a header and the next section header is skipped, including keys and values it could not parse itself, and `fklua init` never writes one. Everything else in the file is still checked, and an unknown key or an unknown section is an error rather than a line that silently does nothing. The reserved name is `tool` exactly, or a name beginning with `tool.`; `[tools]` and `[toolbox]` are ordinary unknown sections and are rejected. One limit follows from the reader being line-based: a line inside a tool section that looks like `[a-section-header]` ends the tool section, so keep nested TOML tables under the tool's own `tool.<name>.` prefix.

## fklua.lock

Derived, never hand-edited. `fklua lock` records the API version, a hash of the pinned `runtime-api.json`, and a hash of the generated bindings. `fklua lock --check` is the CI form, and each mismatch gets its own message: the pin moved without regeneration, the description changed underneath a pinned version, or generated code was edited by hand.

## The Go guest scaffold

`fklua init` writes three files under `guest/go/`, and the location is load-bearing: it is where `gen-bindings` writes the bindings and what `fklua lock` hashes.

- **`go.mod`**: the guest is its own Go module because `//go:wasmimport` is rejected outside `GOARCH=wasm`, so guest source cannot live in a module the host toolchain also builds. The default flow is one `go mod tidy`; `--guest-module PATH` at init time writes a `replace` onto a local FkLua checkout instead.
- **`gc.go`**: the collector import. Under any `-gc` except `custom` the `fkgc` package is empty (no symbols, no state) and this file costs a leaking build nothing, which is why the import is unconditional. Under `-gc=custom` it is what supplies the seven runtime hooks TinyGo's custom-GC seam requires: delete it and a `-gc=custom` build fails to link with `missing core function "runtime.free"`, an error that names neither this file nor the flag. Keep the import.
- **`main.go`**: yours from the first minute. It shows the two simplest hooks (`fk_on_init`, `fk_on_tick`), allocates something so the collector is observable, and paces the collector with one `fkgc.CollectIfNeeded()` call per tick. Its comments answer the questions this page raises abstractly; read them before deleting them.

## The Rust guest scaffold

`fklua init --lang rust` writes `guest/rust/` as a two-member cargo workspace: the generated `fkapi` crate and your guest crate beside it.

- The workspace `Cargo.toml` lists `fkapi` as a member **before it exists**. `fklua gen-bindings` writes that crate, and it is the next step `init` prints; until you run it, the member is a directory that is not there. This is deliberate: a stub crate that compiles is a stub somebody ships.
- The collector is a cargo feature on the `fk` crate, passed on the command line (`cargo build --features fk/fkgc`) rather than declared in the guest's `Cargo.toml`. Cargo's v2 resolver unifies declared features across every crate built in one invocation, so a declared feature would silently turn the collector on for other crates in the same build. There is no import to add and no second flag; `fk` owns the single `#[global_allocator]` site and the feature chooses what backs it. [Why a Rust guest has a garbage collector at all](memory.md) is its own section.
- `src/lib.rs` is yours, on the same terms as the Go `main.go`.
- The release profile ships with `panic = "abort"` (nothing can unwind across the wasm boundary), `opt-level = "s"` and `lto = true` (module size is game load time, and LTO is what lets event-id constants reach `fk.subscribe` so the packaged event table can be pruned). They are requirements, not preferences.

## The bindings: fkapi

`fklua gen-bindings` reads `lang` and `api` from the manifest and writes the Factorio API bindings: for Go, the package at `guest/go/fkapi/`; for Rust, the whole crate at `guest/rust/fkapi/` (manifest, `lib.rs` and the generated source). Inside are typed wrappers for every bindable member of the pinned API, a payload struct and reader for every event, `defines` accessors, subscription helpers and filters. Generated code is never edited by hand; change the pin or update FkLua and regenerate. `fklua gen-bindings --check` verifies committed bindings match what would be generated, which is the CI gate.

Every generated binding set also exports a **pin stamp**, one exported function named `fk_api_pin_<version>`. `fklua mod` refuses to package a guest whose stamp is not the pin being packaged, naming both versions. This matters because member, event and define ids are dense per-version indices: bindings from one description packaged against another call the wrong members, silently wherever the shapes line up. A guest with no stamp (bindings generated before stamps existed) is packaged unchanged.

If your project vendors an FkLua checkout and pins a different API version than the checkout's committed bindings carry, `fklua gen-bindings --into DIR` regenerates the checkout's bindings at your project's pin, in every language the manifest declares.

Changing `api` in the manifest is the whole of a pin move: `fklua gen-bindings && fklua lock`, both run in your project. Two files that look like they belong to that move do not. `api/<version>/runtime-api.json` is part of the compiler, so it is read from the FkLua installation and never copied into your project; and `api/<version>/census.json` beside it is the compiler's own record of what its generators made of that description, which nothing in a mod build reads. Neither is written from a mod project, and neither can make `gen-bindings --check` fail there. If the census in your FkLua installation is behind the generator that just ran, you get one notice saying so and naming the checkout: it is a fact about the toolchain, worth reporting upstream and harmless to your build.

## The packaged mod

`fklua mod your-guest.wasm` writes `<name>_<version>/` (or a zip with `--zip`), which Factorio loads like any other mod:

| File | What it is |
|---|---|
| `info.json` | rendered from `[mod]` in the manifest |
| `control.lua` | the runtime: dispatch, persistence, the collector's pacing driver, the hook wiring |
| `fk_module.lua` | your guest, compiled to Lua and wrapped as a factory |
| `fk_abi.lua` | the handle table and marshalling layer the runtime requires |
| `fk_api_gen.lua` | the member and event tables, pruned to the ids your guest provably calls |

A mod whose settings and data stages are also a guest ships two more files, plus one per stage hook. None of them appears without `data_module`:

| File | What it is |
|---|---|
| `fk_data.lua` | the settings and data stage shim, hand-written and copied in verbatim like `fk_abi.lua` |
| `fk_data_module.lua` | your data guest, compiled to Lua and wrapped as a factory |
| `settings.lua`, `data.lua`, `data-updates.lua`, `data-final-fixes.lua` | one per stage hook the data module exports, and none for a hook it does not |

A mod that declares `[scenarios]` ships one more file per scenario, `scenarios/<name>/control.lua`, and none at all without the key.

### A mod with no control stage

Factorio requires `info.json` and nothing else, so a mod that is only prototypes is an ordinary mod: a compatibility shim, a stand-in, anything whose whole job is `data.raw`. The control guest is optional whenever the mod has a data module, so leave the positional argument out:

```sh
fklua mod --data-module dist/data.wasm --name my-shim --version 1.0.0 --author me
```

or, with `data_module` in the manifest, `fklua mod` on its own. Such a package ships `info.json`, `fk_abi.lua`, `fk_data.lua`, `fk_data_module.lua`, one file per stage hook, and whatever `--include` carries. `control.lua`, `fk_module.lua` and `fk_api_gen.lua` are the three files that describe a running program, and none of them appears. `fk_abi.lua` does, because `fk_data.lua` requires it for the codec that carries a prototype table across the boundary.

`--persist`, `--gc` and `--fuel` describe how a control guest is compiled, so passing one here is refused rather than ignored: a data module is always compiled `--persist=none` and `-gc=leaking`, because it runs once and dies with the Lua state that built it. The refusal is on the flag alone. A `gc` key in the manifest is a statement about the mod that manifest describes, so one project can still package a data-only mod from the command line beside a collected one.

Giving neither a control module nor a data module is an error: the command needs something to package.

The build output states what it did: the size of the Lua it wrote, the modes it used, each guest export it wired to a Factorio hook, and the pruning result (`API 2.0.77: 1 members, pruned from 4259`). An id the scan cannot prove constant ships the full table instead, which is a bigger mod but never a broken one, and the output says so.

A mod with a hand-written data stage ships it through `data = "DIR"` under `[mod]` (or `fklua mod --include DIR`); the directory's contents are merged into the package, and a name collision with a generated file is an error at package time rather than a mod that is silently wrong in game. A mod whose data stage is a guest declares `data_module` under `[fklua]` instead, and can run both at once while it moves from one to the other: see [`docs/data-stage.md`](data-stage.md). The packaged output itself is disposable: edit the guest or the manifest and run `fklua mod` again.

### Scenarios a mod ships

A scenario's `control.lua` is a full control stage in its own Lua state, so it is not somewhere a guest can be compiled to. The base game's own convention for a mod-shipped scenario is a one-line require into the mod's tree, and that is exactly the `control.lua` `fklua mod` already writes for the mod root. `[scenarios]` says where to put a copy of that line:

```toml
[scenarios]
freeplay-plus = ["@control"]
```

writes `scenarios/freeplay-plus/control.lua` containing `require("__my-mod__/control")`, so picking that scenario in the New Game menu runs the mod's guest. The cross-mod path is what makes it work from a scenario's own directory; a bare `require("control")` would look beside the scenario instead.

Each value is an ordered chain of requires, exactly like `[stages]`, with `"@control"` standing for the mod's own control stage. A scenario that also needs Lua of its own names it in the order it wants:

```toml
[scenarios]
custom = ["__my-mod__/scenario-setup", "@control"]
```

A chain with no `"@control"` is a pure-Lua scenario and is legal; a chain that names it in a mod with no control guest is an error at package time, because that scenario would load and do nothing. Everything else a scenario directory holds -- `description.json`, `blueprint.zip`, a locale directory, a saved map -- is authored, and comes in through `data` (or `--include`) like any other file. An included file that would overwrite a generated shim is an error, with the same remedy `[stages]` gives: put the hand-written file back into the chain.

### Packaging several mods from one manifest

Every `[mod]` key has a flag, so one project can package more than one mod: point `fklua mod` at a different guest and override the identity on the command line. Dependencies work the same way, with one rule worth stating outright. `--dependency DEP` is repeatable, and the values given **replace** `[mod] dependencies` rather than adding to them:

```sh
fklua mod observer.wasm --name my-observer --dependency "base >= 2.0.0" --dependency "? quality"
```

Replacement rather than appending is what lets a packaging declare a *smaller* list than the manifest's, including an empty one. That case is real: Factorio loads mods in dependency order, so a mod that has to run before another must not declare a dependency on it. `--dependency ""` on its own means no dependencies at all, and leaves the key out of `info.json` entirely; combining it with a real value is refused. Giving no `--dependency` at all packages the manifest's list unchanged.

Values are passed through verbatim. Factorio is the authority on its own dependency syntax, so nothing here parses it.

## The project, machine-readable: fklua meta

`fklua meta --json` writes one JSON document describing the project in the working directory, so a tool that drives fklua does not have to read `fklua.toml` itself or re-derive the defaults fklua applies to it. `--json` is required: this command is a data interface and has no human-facing form, and the flag is how a caller says it expects standard output to be one JSON document.

```sh
fklua meta --json
```

The document has five top-level keys, all always present.

| Key | What it is |
|---|---|
| `fklua` | the compiler version, the same string `fklua version` prints |
| `manifest` | the manifest as written: raw values, with an empty string, an empty list or an empty object wherever the author wrote nothing |
| `effective` | the same field set after every default `fklua mod` applies |
| `package` | the identity Factorio finds the built mod by |
| `guest` | the per-language guest layout, keyed by language |

`manifest` and `effective` carry the same fields, so the two blocks can be compared directly: `name`, `version`, `title`, `author`, `description`, `factorio_version`, `data`, `dependencies`, `api`, `lang`, `gc`, `data_module`, `stages`, `scenarios`. `dependencies` and `lang` are always arrays and `stages` and `scenarios` are always objects, empty rather than `null`, so a consumer can iterate them without checking first.

These are the rules `effective` applies, and they are the rules `fklua mod` applies:

| Field | Rule when the manifest does not set it |
|---|---|
| `title` | the mod's `name` |
| `author` | `"unknown"` |
| `factorio_version` | the default API pin's `major.minor` series. It is the DEFAULT pin's series and not this project's: `api` never reaches this field, so a project pinned at a 2.1.x description with no `factorio_version` key still declares `2.0`. Set the key when the two axes come apart |
| `lang` | `["go"]` |
| `gc` | `"leaking"` |
| everything else | no default; the effective value is the raw one |

**An absent `gc` key means `"leaking"`, never `"collected"`.** `fklua init` writes `gc = "collected"` into a new project, which makes a manifest without the key look newer rather than older, but the compile-flag default is deliberately unchanged so that an existing build never becomes a compile error naming a flag its author did not choose. `effective.gc` is always one of `"leaking"` or `"collected"`, computed by the same call `fklua mod` makes, and `manifest.gc` is empty when the author wrote nothing, so a tool can still tell "leaking by default" from "leaking on purpose".

One value in `manifest` is not quite as written. An absent `lang` key is normalized to `["go"]` while the file is being read, before this command sees it, so `manifest.lang` reports `["go"]` for a file with no `lang` line. The effective value is the same either way; the only fact that cannot be recovered is whether the author typed it.

`package` is `dir` (`<name>_<version>`, the directory Factorio expects) and `zip` (the same name with `.zip`).

`guest` has one entry per language in `effective.lang`, and a language the project does not build has no key at all. Every path is relative to the project root and uses forward slashes.

| Field | Language | What it is |
|---|---|---|
| `dir` | both | the guest source directory: `guest/go` or `guest/rust` |
| `bindings` | both | where `fklua gen-bindings` writes, and what `fklua lock` hashes by exact name |
| `wasm` | Go | the conventional artifact at the project root, `<name>.wasm`, which is where the `tinygo build` line `init` prints puts it and what `fklua mod` is then given |
| `wasm` | Rust | the artifact a release `cargo build` writes, under `guest/rust/target/wasm32-unknown-unknown/release/`. A cdylib is named after the crate with dashes mapped to underscores, so a crate `my-mod-guest` builds `my_mod_guest.wasm` |
| `crate` | Rust | the cargo package name, the mod name sanitized with a `-guest` suffix |
| `crate_dir` | Rust | that crate's directory inside the workspace |

The command reads `fklua.toml` from the working directory and errors without one, which is where it differs from `fklua mod` and `fklua compile`: those take their identity from flags when there is no manifest, and this command has nothing to fall back to, since the document is a description of a manifest. A caller one directory above its project gets a failure rather than a plausible answer. A value the toolchain itself would reject is refused here too, with the same message, so a `gc` the compiler will not accept or a `lang` with no generator is reported by this command rather than discovered later in a build.

`[tool]` and `[tool.<name>]` sections never appear in the output. They belong to the tools that write them, and fklua does not read their contents.

Every field name in the document is stable. Parse the JSON rather than any prose fklua prints.

## A reference to read

`fklua docs` renders a browsable API reference for the pinned description in your language's names (`docs/api-go.md` or `docs/api-rust.md`; `-o` chooses the directory). It is generated from the same description the bindings come from, so it matches what your `fkapi` actually offers.
