package guest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// THE LINE BUILDER AND THE VALUE DUMPER, in both languages, against one
// expectation.
//
// fklog is HAND-WRITTEN in both trees and nothing generates it, so `census.json`
// cannot see it, `gen-bindings --check` cannot see it, and the two copies can
// drift in exactly the way this repo has already measured: at least nine guests
// rolled their own line builder and one of them grew a real rounding
// divergence. A shared golden is the only mechanism available, and it is
// TestBothDataGuestLibrariesMakeTheSameCalls' shape applied to a library rather
// than to a stage.
//
// Value.Dump is in the generated fkapi PREAMBLE and not in fklog, deliberately:
// fklog depends on fk alone, because fkapi is generated and stamped with an API
// pin and a line builder has no business dragging one into a consumer that only
// wanted it. What joins them is a borrowed buffer -- fklog.Tail lends, the
// dumper writes, fklog.Advance records -- and that seam is what these legs run.
func TestTheLineBuilderAndTheDumperAgreeInBothLanguages(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		if ok, why := guest.Available(); !ok {
			t.Skipf("skipping: %s", why)
		}
		p := filepath.Join(t.TempDir(), "logdump.wasm")
		if err := guest.Build(filepath.Join(repoRoot(t), "guest", "go"), "./examples/logdump", p); err != nil {
			t.Fatalf("building the Go guest: %v", err)
		}
		checkLogDumpGuest(t, p)
	})
	t.Run("rust", func(t *testing.T) {
		if ok, why := guest.RustAvailable(); !ok {
			t.Skipf("skipping: %s", why)
		}
		p, err := guest.BuildRust(filepath.Join(repoRoot(t), "guest", "rust"), "logdump",
			filepath.Join(t.TempDir(), "cargo"))
		if err != nil {
			t.Fatalf("building the Rust guest: %v", err)
		}
		checkLogDumpGuest(t, p)
	})
}

// logDumpWant is the transcript both languages owe.
//
// Every line is order-independent: the tier-2 value is built by the GUEST rather
// than read back off a host table, so its pair order is the program's own.
var logDumpWant = []string{
	"LOG nums 0 42 -7 true false",
	// The signed edge. -v overflows at the most negative value and prints it as
	// itself, which is the divergence one hand-written copy actually grew.
	"LOG edge -9223372036854775808 18446744073709551615",
	// One decimal, ROUNDED HALF AWAY FROM ZERO, so 1.25 is 1.3; including the
	// carry, so 9.96 is 10.0 and not 9.10; and a negative that rounds to zero
	// keeps its sign, which is what the caller asked to be told.
	"LOG f1 1.3 10.0 -0.0",
	// Truncation over growth: 2,000 bytes into a 512-byte buffer stops at 512.
	"LOG trunc 512",
	// The dumper, through fklog's own tail. A string key is bare and a number
	// key takes the [k] form, so the two cannot read alike; an integral number
	// has no fractional part and 1.5 keeps one digit rather than three.
	`LOG dump {name="belt", count=42, ratio=1.5, on=true, gone=nil, list=[1, "two", false], inner={deep=7}, [7]="seven"}`,
	// A scalar at the top level, and the two empty containers, which is where a
	// recursive renderer most often gets a separator wrong.
	"LOG scalars -0.5 nil [] {}",
	// The dumper truncates too, and reports what fitted rather than what it
	// wanted: a quote and seven of the ten digits.
	`LOG dumptrunc 8 "0123456`,
}

func checkLogDumpGuest(t *testing.T, wasmPath string) {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packageLogDumpGuest(t, wasmPath)
	out, err := h.RunString(fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
defines = { events = {} }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-logdump",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
game = {}
helpers = {}
require("control")
handlers.on_init()
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, out)
	}

	got := strings.Split(strings.TrimSpace(out), "\n")
	for i := range got {
		got[i] = strings.TrimSpace(got[i])
	}
	if len(got) != len(logDumpWant) {
		t.Fatalf("expected %d lines, got %d:\n%s", len(logDumpWant), len(got), out)
	}
	for i := range logDumpWant {
		if got[i] != logDumpWant[i] {
			t.Errorf("line %d:\n  got  %s\n  want %s", i+1, got[i], logDumpWant[i])
		}
	}
}

func packageLogDumpGuest(t *testing.T, wasmPath string) string {
	t.Helper()
	root, tmp := repoRoot(t), t.TempDir()
	raw, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := wasm.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(mod)
	if err != nil {
		t.Fatal(err)
	}
	src, err := luagen.EmitModuleWith(im, luagen.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := factorio.LoadAPI(filepath.Join(root, "api",
		factorio.DefaultAPIVersion, "runtime-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	report := factorio.GenerateMembers(a)
	events := factorio.GenerateEvents(a)
	used, complete := factorio.UsedMembers(im)
	if !complete {
		t.Fatal("a member id was not a compile-time constant, so the scan broke")
	}
	usedEv, evComplete := factorio.UsedEvents(im)
	if !evComplete {
		t.Fatal("an event id was not a compile-time constant, so the scan broke")
	}
	table, err := report.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fk-logdump", Version: "0.1.0", Title: "FkLua fklog and Value.Dump",
			Author: "FkLua", FactorioVersion: factorio.DefaultFactorioVersion,
		},
		Chunk: src, APITable: table,
	}
	for _, e := range im.Exports {
		pkg.Exports = append(pkg.Exports, e.Name)
	}
	dir, err := pkg.WriteDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
