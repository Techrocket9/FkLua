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
| `dependencies` | passed to `info.json` verbatim, in Factorio's own syntax: `"base >= 2.0.0"` required, `"? other"` optional, `"! other"` conflicts |

`[fklua]` configures the toolchain:

| Key | What it is |
|---|---|
| `api` | the API pin: which committed `runtime-api.json` the bindings and the packaged member table come from. See [the version axes](factorio-api.md) |
| `lang` | the guest language(s) to generate bindings for: `["go"]`, `["rust"]`, or both |
| `gc` | how the guest heap is managed: `"collected"` (what `init` writes) or `"leaking"`. This key must agree with how the guest was built; a mismatch is a refusal at package time that names both sides. An absent key means "nobody said", which keeps pre-existing projects byte-for-byte unchanged. See [Memory, the collector and the save](memory.md) |

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

## The packaged mod

`fklua mod your-guest.wasm` writes `<name>_<version>/` (or a zip with `--zip`), which Factorio loads like any other mod:

| File | What it is |
|---|---|
| `info.json` | rendered from `[mod]` in the manifest |
| `control.lua` | the runtime: dispatch, persistence, the collector's pacing driver, the hook wiring |
| `fk_module.lua` | your guest, compiled to Lua and wrapped as a factory |
| `fk_abi.lua` | the handle table and marshalling layer the runtime requires |
| `fk_api_gen.lua` | the member and event tables, pruned to the ids your guest provably calls |

The build output states what it did: the size of the Lua it wrote, the modes it used, each guest export it wired to a Factorio hook, and the pruning result (`API 2.0.77: 1 members, pruned from 4257`). An id the scan cannot prove constant ships the full table instead, which is a bigger mod but never a broken one, and the output says so.

A mod with a data stage ships it through `data = "DIR"` under `[mod]` (or `fklua mod --include DIR`); the directory's contents are merged into the package, and a name collision with a generated file is an error at package time rather than a mod that is silently wrong in game. The packaged output itself is disposable: edit the guest or the manifest and run `fklua mod` again.

## A reference to read

`fklua docs` renders a browsable API reference for the pinned description in your language's names (`docs/api-go.md` or `docs/api-rust.md`; `-o` chooses the directory). It is generated from the same description the bindings come from, so it matches what your `fkapi` actually offers.
