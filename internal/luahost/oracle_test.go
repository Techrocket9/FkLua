package luahost

import (
	"errors"
	"os"
	"testing"
)

// THE ONE TEST THAT FAILS WHEN THE ORACLE IS MISSING, so that `go test ./...`
// cannot come back green for a run that measured nothing.
//
// The problem this exists for is structural rather than anybody's mistake.
// `/bin/` is gitignored -- correctly; lua52f is a 217 KB build artefact -- so
// every fresh `git worktree add` and every fresh clone starts without it. About
// thirty tests across internal/luagen, internal/spectest, internal/factorio,
// internal/guest and this package respond to a missing oracle by SKIPPING, which
// is the right thing for each of them to do individually and the wrong outcome
// in aggregate: `go test` prints nothing for a skip unless it is passed `-v`, so
// the transcript reads
//
//	ok  	github.com/Techrocket9/fklua/internal/guest	0.412s
//
// for a package whose entire collector suite declined to run. Stage D found the
// collector suite in that state in a fresh worktree and it was indistinguishable
// from a pass.
//
// The fix is not to make thirty tests fail. Each of them is right to skip: a
// contributor working on the emitter's AST who has not built Lua should still be
// able to run the type checker's tests. The fix is that the ABSENCE ITSELF is
// reported once, loudly, by a test that fails -- so the run is red, the reason is
// on screen, and the remedy is in the message.
//
// It is deliberately not opt-out-able by an environment variable. An escape
// hatch here would be reached for by exactly the person this test exists to
// stop, and the real escape hatch already exists and is one command.
func TestTheOracleIsBuilt(t *testing.T) {
	if _, err := Find(); err != nil {
		if errors.Is(err, ErrNotBuilt) {
			t.Fatalf("THE HOST-SIDE ORACLE IS MISSING, so most of this repo's "+
				"tests just SKIPPED and the run above reads like a pass.\n\n  %v\n\n"+
				"lua52f is Lua 5.2.1 patched to Factorio's sandbox. It is what "+
				"every emitter, spectest, ABI, persistence and collector test "+
				"measures against, and it is not substitutable -- the installed "+
				"lua is 5.5, whose integer subtype makes %%, overflow and "+
				"string.pack behave differently from the game.", err)
		}
		t.Fatalf("locating lua52f: %v", err)
	}
}

// And that the one thing which CAN silently substitute a different interpreter
// points at something real. LUA52F takes precedence over the repo's own binary,
// so a stale value in a shell profile would redirect every measurement in the
// repo at whatever it names, with no other symptom.
func TestTheLUA52FOverridePointsAtSomething(t *testing.T) {
	env := os.Getenv("LUA52F")
	if env == "" {
		t.Skip("LUA52F is not set, so bin/lua52f is what everything uses")
	}
	if _, err := os.Stat(env); err != nil {
		t.Fatalf("LUA52F=%s is set but does not exist (%v). It OVERRIDES "+
			"bin/lua52f, so every host-side measurement in this repo would be "+
			"taken against whatever it names -- unset it or point it at a real "+
			"lua52f", env, err)
	}
	t.Logf("LUA52F=%s is overriding bin/lua52f for this run", env)
}
