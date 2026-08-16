.PHONY: all test lint lua52f check-lua52f spectest bench optbench probe guest clean clean-rust

LUA_DIR := third_party/lua-5.2.1
LUA52F  := bin/lua52f

all: lua52f
	go build -o bin/fklua ./cmd/fklua

# `test` DEPENDS ON THE ORACLE, and the dependency is the whole point of the
# line. Without lua52f about thirty tests across five packages skip, `go test`
# prints nothing for a skip, and a package whose entire collector suite declined
# to run reports `ok` — which is the state stage D found a fresh worktree in.
# internal/luahost's TestTheOracleIsBuilt makes `go test ./...` red for anyone
# who bypasses this target; this line means the repo's own entry point never
# gets there.
# ...AND `go test ./...` DOES NOT REACH sdk/go, because it is a separate module.
# That module holds the external fkipc SDK and the wire-vector golden's only
# reader-and-generator, so leaving it to `go test ./...` is a golden nothing
# regenerates and nothing checks. It is pure Go: no oracle, no toolchain, ~1 s.
# guest/go is separate too, and its HOST-BUILDABLE packages run here SCOPED:
# `go test ./fkipc/...` -- a bare `./...` cannot compile off-target, because fk
# and fkapi are //go:wasmimport declarations with no bodies. That scoped slice
# is the guest fkipc state machine, the wire codec, and the Go half of the
# outbound-seam text property; none of it needs a toolchain, an oracle, or the
# network. The wasm-only rest of the module is exercised by internal/guest
# through a real TinyGo build, as before.
#
# NOR DOES IT REACH guest/rust, and until this line existed nothing did: 68
# host-side tests over the Rust IPC codec, the link state machine and the OTHER
# reader of the committed wire-vector golden ran in neither this target nor CI,
# so the only thing standing between the two implementations diverging and
# nobody noticing was a person remembering a command. That is the "a gate
# nobody added" half of this repo's pair -- visibly absent rather than reported
# green -- and it costs seconds, because fkipc's fkapi dependency is
# target-gated to `cfg(target_family = "wasm")` and the crate therefore has NO
# host dependencies at all: no network, no wasm target, no oracle.
#
# -p IS LOAD-BEARING AND IS NOT A NARROWING. A bare `cargo test` at the
# workspace root cannot compile -- fk and fkgc name core::arch::wasm32 and the
# host has no such module -- so anything that widens this line owes the crate
# name. fkipc/Cargo.toml says the same thing next to the dependency that makes
# it true.
#
# A MISSING cargo IS A NOTICE HERE AND NOT A FAILURE, which is a deliberate
# exception to this repo's "absence must be loud" rule rather than a lapse from
# it, for three reasons. The oracle's treatment does not transfer: make BUILDS
# lua52f from committed inputs and cannot install a toolchain. TinyGo's does,
# and TinyGo's channel is a FAILING GUARD TEST rather than a Makefile branch --
# internal/guest.TestTheRustToolchainIsAvailable is that guard's Rust twin, it
# hard-fails on a machine with no Rust, it is deliberately NOT exempted by
# FKLUA_NO_GUEST_TOOLCHAIN, and it runs in `go test ./...` two lines above, so
# `make test` is already red before this line is reached. Absence is reported
# once, by the guard that owns it, with the remedy and the -short opt-out in
# its message; a second weaker failure here would only duplicate it worse.
# And the residual this branch really covers -- cargo absent while rustc is
# present, which RustAvailable never asks about -- reaches a channel the Go
# guards measured and could not have: `go test` CAPTURES a passing test's
# output and discards it, which is why that middle ground was built and thrown
# away, while make writes straight to the terminal. Last line of the recipe, so
# it is the last thing on screen.
test: $(LUA52F)
	go test ./...
	cd sdk/go && go test ./...
	cd guest/go && go test ./fkipc/...
	@if command -v cargo >/dev/null 2>&1; then \
	  echo "cd guest/rust && cargo test -p fkipc"; \
	  cd guest/rust && cargo test -p fkipc; \
	else \
	  echo ""; \
	  echo "======================================================================"; \
	  echo "  NOT RUN: cargo test -p fkipc -- cargo is not on PATH."; \
	  echo ""; \
	  echo "  68 host-side tests did not run: the Rust fkipc wire codec, the"; \
	  echo "  link state machine, and the Rust half of the committed golden"; \
	  echo "  testdata/ipc/wire-vectors.txt. That golden is the ONLY"; \
	  echo "  cross-language pin needing no toolchain and no Factorio, and"; \
	  echo "  with this leg absent only the Go half of it was just checked."; \
	  echo ""; \
	  echo "  Install cargo (https://rustup.rs) and re-run make test."; \
	  echo "======================================================================"; \
	  echo ""; \
	fi

lint:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...

# ---------------------------------------------------------------------------
# lua52f — Lua 5.2.1 patched to match Factorio's sandbox.
#
# This is the host-side oracle for every non-Factorio test. It is NOT optional
# and NOT substitutable: Homebrew has no lua@5.2, and the installed lua 5.5 has
# an integer subtype that makes %, overflow and string.pack behave differently
# from Factorio's doubles-only 5.2.1. Testing against 5.5 silently passes code
# that breaks in game.
#
# IN A GIT WORKTREE THIS COPIES RATHER THAN BUILDS. `/bin/` is gitignored, so
# every `git worktree add` starts without the oracle and every host-side test in
# the new tree skips — silently, because `go test` prints nothing for a skip.
# Fetching and rebuilding Lua from source to reproduce a binary the main
# checkout already has is minutes of nothing, so if `git rev-parse
# --git-common-dir` points somewhere else, the main checkout's binary is copied
# across. It is the same file: lua52f is a pure function of the tarball and the
# committed patches, both of which are shared by every worktree.
# ---------------------------------------------------------------------------
lua52f: $(LUA52F)

$(LUA52F): $(LUA_DIR)/fetch.sh $(wildcard $(LUA_DIR)/patches/*.patch)
	@mkdir -p bin
	@main="$$(git rev-parse --git-common-dir 2>/dev/null)"; \
	 main="$${main%/.git}"; \
	 if [ -n "$$main" ] && [ "$$main" != "$$(git rev-parse --show-toplevel 2>/dev/null)" ] \
	    && [ -x "$$main/$(LUA52F)" ]; then \
	   cp "$$main/$(LUA52F)" $(LUA52F); \
	   echo "copied $(LUA52F) from the main checkout at $$main"; \
	 else \
	   $(LUA_DIR)/fetch.sh && cp $(LUA_DIR)/build/src/lua $(LUA52F) && \
	   echo "built $(LUA52F)"; \
	 fi

# Asserts lua52f actually is sandbox-shaped. If this drifts from Factorio, every
# host-side test result is a lie, so it runs in CI on every push.
check-lua52f: $(LUA52F)
	@$(LUA52F) $(LUA_DIR)/sandbox_check.lua

# ---------------------------------------------------------------------------
# spectest — the official WebAssembly conformance suite, the primary correctness
# metric. testdata/spec holds committed wast2json output, so this needs no WABT.
# Self-activating: silent no-op until testdata/spec is populated at M1.
# ---------------------------------------------------------------------------
spectest: $(LUA52F)
	@if [ -z "$$(ls -A testdata/spec 2>/dev/null)" ]; then \
		echo "spectest: testdata/spec is empty — suite lands at M1, skipping"; \
	else \
		go run ./cmd/fklua spectest; \
	fi

bench: $(LUA52F)
	@go run ./cmd/fklua bench

# What the optimizer is worth. Distinct from `bench` on purpose: the M0 kernels
# are hand-written Lua standing in for generated code, so they establish the
# ceiling and do not move when the emitter improves. These compile real modules
# with the real compiler at every -opt level.
optbench: $(LUA52F)
	@go run ./cmd/fklua bench --opt

probe:
	@go run ./cmd/fklua probe

# ---------------------------------------------------------------------------
# guest — build the M4 guest, package it, and run it in a real Factorio.
#
# The only check that a packaged mod actually LOADS. lua52f models the sandbox
# and the end-to-end test drives control.lua against stand-ins for the game
# globals, but neither is Factorio: the mod format, `require` resolution and the
# log plumbing are outside what the oracle can speak to.
# ---------------------------------------------------------------------------
guest:
	@./scripts/run-guest.sh

# ---------------------------------------------------------------------------
# clean-rust — delete guest/rust/target.
#
# IT IS GITIGNORED, WHICH IS EXACTLY WHY IT MISLEADS. `git status` is clean, a
# branch switch leaves it untouched, and cargo keeps one artifact path per
# (package, profile) whatever features built it -- so the tree holds rlibs from
# a crate that no longer exists, a feature set nobody is passing now, or the
# other arm of an A/B. A Rust agent chased symbols out of exactly that.
#
# scripts/run-guest.sh writes here (target/leaking and target/collected);
# internal/guest and scripts/run-roundtrip.sh set CARGO_TARGET_DIR elsewhere on
# purpose, so this target is the checked-out tree's copy and not the tests'.
#
# Separate from `clean` because it is minutes of rebuild and `clean` is reached
# for casually. Reach for this one when a Rust symbol does not match its source.
# ---------------------------------------------------------------------------
clean-rust:
	rm -rf guest/rust/target bench/guests/rust/target

clean:
	rm -rf bin $(LUA_DIR)/build $(LUA_DIR)/lua-*.tar.gz
