package main

// The machine-readable half of `fklua mod`: --report FILE writes one JSON
// document describing what the packaging decided, so a tool can read the
// verdicts this command has always printed as prose.
//
// WHY IT EXISTS. Everything a driving tool needs from a packaging -- the
// pruning verdicts ("all 219 events -- an event id was not a compile-time
// constant"), the pin and signature outcomes, the wired hooks -- was stdout
// and stderr prose, and prose is not a data interface: the one consumer this
// was built for (the fklua-mod-toolkit) forbids scraping it, correctly,
// because a reworded sentence is a silently broken scraper. This is `api
// check --json`'s lesson applied to the one command whose output nobody could
// consume, and `api diff --json PATH` / `bench --json PATH` are the precedent
// for writing a document to a FILE the caller names.
//
// THE CONTRACT, and each clause has a test:
//   - The report is written on SUCCESS and on REFUSAL alike. A refusal is
//     exactly when a driving tool needs structure -- it is what becomes a
//     diagnostic with a remedy -- so a report that only existed for green
//     builds would be useless for the half it was asked for. `ok` says which,
//     and `refusal` carries a stable `kind` plus the human message.
//   - The field-name SET is part of the contract and is pinned as raw JSON,
//     because a typed unmarshal is silent about a rename (api check's rule).
//   - A list is [] and never null (a property of the BYTES, same rule).
//   - Resolved values are echoed with their SOURCE -- the api version and
//     where the pin came from, the engine series and what chose it -- because
//     both have moved under this repo before and a tool cannot know which
//     question it asked without the echo.
//
// WHAT IS DELIBERATELY NOT HERE: the human prose. The stdout lines are
// untouched to the byte -- they are what every build log and script greps --
// and the report is not printed, only written. A refusal's remedy stays in
// the message text; `kind` exists so a tool can attach its own remedy without
// parsing sentences.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// refusalError tags an error a subcommand chose deliberately with a stable
// machine kind. The kinds are contract: a tool switches on them, so renaming
// one is a break even though every message stays free to move.
type refusalError struct {
	kind string
	err  error
}

func (r refusalError) Error() string { return r.err.Error() }
func (r refusalError) Unwrap() error { return r.err }

// refuse wraps a refusal with its kind; a nil error passes through so call
// sites can wrap unconditionally.
func refuse(kind string, err error) error {
	if err == nil {
		return nil
	}
	return refusalError{kind: kind, err: err}
}

type reportRefusal struct {
	// Kind is stable and machine-readable: "api_pin", "gc", "data_module",
	// "emit", "package", or "error" for anything unclassified.
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type reportCount struct {
	// Complete says the constant scan proved every id, so the table was
	// pruned; false is the "ships the full table" arm -- a bigger mod, never a
	// broken one, and exactly the verdict a driving tool wants to surface.
	Complete bool `json:"complete"`
	// Shipped is the row count actually packaged: the proved set when
	// Complete, the whole table when not. Total is the description's count.
	// Both are 0, and Complete is vacuously true, when no table was attached
	// at all (api.table_attached).
	Shipped int `json:"shipped"`
	Total   int `json:"total"`
}

type modReport struct {
	path string // where to write it; "" means --report was not typed

	OK      bool           `json:"ok"`
	Refusal *reportRefusal `json:"refusal"`

	API struct {
		Version string `json:"version"`
		Source  string `json:"source"`
		// TableAttached is false for a guest that calls no member, subscribes
		// to no event and reads no define -- and for a data-only packaging.
		// The pruning totals are 0 there: not "everything proved", "nothing to
		// prove".
		TableAttached bool `json:"table_attached"`
		TableBytes    int  `json:"table_bytes"`
	} `json:"api"`

	Build struct {
		ControlModule         bool   `json:"control_module"`
		DataModule            bool   `json:"data_module"`
		GC                    string `json:"gc"`
		Persist               string `json:"persist"`
		Opt                   string `json:"opt"`
		NaN                   string `json:"nan"`
		Fuel                  int    `json:"fuel"`
		FactorioVersion       string `json:"factorio_version"`
		FactorioVersionSource string `json:"factorio_version_source"`
	} `json:"build"`

	Pruning struct {
		Members reportCount `json:"members"`
		Events  reportCount `json:"events"`
		Defines reportCount `json:"defines"`
	} `json:"pruning"`

	// Pin is the description-identity check (fk_api_pin_*). "ok" when the
	// guest's stamp matches the packaged version, "absent" when the guest
	// carries none (bindings older than the stamp, or no bindings at all),
	// "not_checked" when packaging never reached the check -- a guest with no
	// table to attach, or a data-only mod. A MISMATCH is a refusal, not a
	// status: the run fails and `refusal.kind` is "api_pin".
	Pin struct {
		Status   string   `json:"status"`
		Guest    []string `json:"guest"`
		Packaged string   `json:"packaged"`
	} `json:"pin"`

	// Signature is the generation check (fk_api_sig_*), and unlike the pin a
	// mismatch here is a WARNING and stays one: "mismatch" is a status because
	// an id that only moved by appending is still correct. See
	// warnAPISignature for the whole argument.
	Signature struct {
		Status   string   `json:"status"`
		Guest    []string `json:"guest"`
		Packaged string   `json:"packaged"`
	} `json:"signature"`

	Hooks struct {
		Control []string `json:"control"`
		Data    []string `json:"data"`
	} `json:"hooks"`

	Outputs struct {
		Path            string `json:"path"`
		Zip             bool   `json:"zip"`
		ControlLuaBytes int    `json:"control_lua_bytes"`
		DataLuaBytes    int    `json:"data_lua_bytes"`
	} `json:"outputs"`

	LuaMigrations []string `json:"lua_migrations"`
	Inert         bool     `json:"inert"`
}

// newModReport starts a report whose every list is non-nil, because a list is
// [] and never null -- a property of the bytes, so it is established at
// construction rather than trusted to every population site.
//
// THE PRUNING FLAGS START TRUE, and the first consumer is why. `complete:
// false` is the "ships the full table" warning a driving tool surfaces, and a
// packaging that never runs a scan at all -- a data-only mod, or a refusal
// before attachAPI -- is not that: there is no table for the verdict to be
// about, which `api.table_attached: false` already says. The zero value read
// as a real warning there (FkRecipes' dogfood report called it "correct but
// alarming", and it was not even correct), so the default is the vacuous
// truth and only an actual scan writes a false.
func newModReport() *modReport {
	r := &modReport{}
	r.Pruning.Members.Complete = true
	r.Pruning.Events.Complete = true
	r.Pruning.Defines.Complete = true
	r.Pin.Status = "not_checked"
	r.Pin.Guest = []string{}
	r.Signature.Status = "not_checked"
	r.Signature.Guest = []string{}
	r.Hooks.Control = []string{}
	r.Hooks.Data = []string{}
	r.LuaMigrations = []string{}
	return r
}

// write records the run's outcome and writes the document. Called for success
// and refusal alike; the caller decides what to do when the WRITE itself
// fails (a failed write must not mask a real refusal).
func (r *modReport) write(runErr error) error {
	r.OK = runErr == nil
	if runErr != nil {
		kind := "error"
		var ref refusalError
		if errors.As(runErr, &ref) {
			kind = ref.kind
		}
		r.Refusal = &reportRefusal{Kind: kind, Message: runErr.Error()}
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the report: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(r.path, b, 0o644); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	return nil
}
