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

	"github.com/Techrocket9/fklua/internal/luagen"
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

// reportJumpFunc is one function's row in the jumps object: what its widest
// jump cost and how much room the relay had inside it.
type reportJumpFunc struct {
	Function string `json:"function"`
	// WidestSpanBytes is the widest goto-to-label distance in this function
	// BEFORE the relay ran. It is the number that says how near Lua's wall the
	// guest is; the one after the relay does not.
	WidestSpanBytes int `json:"widest_span_bytes"`
	Stations        int `json:"stations"`
	// WidestSpanAfterRelayBytes is what the ladder left. For a relayed function
	// it reads about one hop whatever the guest's size, because a hop is what
	// the ladder was built to, so it is a property of the relay and not a
	// headroom figure.
	WidestSpanAfterRelayBytes int `json:"widest_span_after_relay_bytes"`
	// WidestBlockBytes is the longest run with no body-level statement boundary
	// in it, taken inside the span this function's fate turns on, and
	// BlockRoomBytes is what is left of one hop over it. This pair is the
	// relay's own ceiling: it goes negative exactly when the relay cannot help
	// and the function is refused. The span it is taken in is the widest one
	// while the relay can still carry the function, and on a refusal the span
	// the relay could NOT carry -- which need not be the widest, because every
	// label is planned on its own. See FuncJumps in internal/luagen.
	WidestBlockBytes int `json:"widest_block_bytes"`
	BlockRoomBytes   int `json:"block_room_bytes"`
}

// reportJumps is one module's entry in the jumps object.
//
// WHY THIS IS HERE AT ALL. `outputs.data_lua_bytes` and the "N bytes of Lua"
// line say how big a module is, and module size is free: a chunk is parsed once
// and never appears in a save. What is not free is CONCENTRATION inside one
// function, and until this the only way an author learned they were near it was
// the refusal. The first library built on this substrate transcribed the
// emitter's scan into a script of its own to find out, and read the relay's hop
// as a headroom figure because that is the number that was visible.
//
// LimitBytes and HopBytes are constants of the emitter rather than measurements
// of this module, so they are filled in even for a module that is not there;
// every measured number is 0 in that case and `relayed` is false.
type reportJumps struct {
	// LimitBytes is the span at which a function is refused, and HopBytes is how
	// far one hop of the relay's ladder may reach.
	LimitBytes int `json:"limit_bytes"`
	HopBytes   int `json:"hop_bytes"`
	// The widest PRE-relay span in the module, and the function carrying it.
	WidestSpanBytes    int    `json:"widest_span_bytes"`
	WidestSpanFunction string `json:"widest_span_function"`
	// Stations is the module's total, over every function the relay carried.
	Stations int `json:"stations"`
	// WidestSpanAfterRelayBytes is the widest span left anywhere in the module.
	WidestSpanAfterRelayBytes int `json:"widest_span_after_relay_bytes"`
	// The block measure, taken over EVERY function: the longest run with no
	// body-level statement boundary in it anywhere in the module, the function
	// carrying it, and what is left of one hop over it. Within a function it is
	// measured inside the widest span while the relay can still carry that
	// function, and inside the span that defeated the relay once one exists.
	// widest_block_function is usually NOT widest_span_function: the longest
	// run tends to sit in a helper, the widest jump in the function that
	// inlined the sections. The case that matters is a loop body longer than
	// a hop in a function whose span is still under the limit.
	WidestBlockBytes    int    `json:"widest_block_bytes"`
	WidestBlockFunction string `json:"widest_block_function"`
	BlockRoomBytes      int    `json:"block_room_bytes"`
	Relayed             bool   `json:"relayed"`
	// RelayedFunctions is every function the relay carried, in emission order.
	// [] and never null.
	RelayedFunctions []reportJumpFunc `json:"relayed_functions"`
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

	// Jumps is Lua's 18-bit jump limit, per module: how near each one came to
	// it, what the relay did, and how much room the relay itself has left.
	Jumps struct {
		Control reportJumps `json:"control"`
		Data    reportJumps `json:"data"`
	} `json:"jumps"`

	LuaMigrations []string `json:"lua_migrations"`
	Inert         bool     `json:"inert"`
}

// jumpEntry renders one module's facts as its report entry.
func jumpEntry(j luagen.ModuleJumps) reportJumps {
	e := reportJumps{
		LimitBytes:                luagen.MaxJumpSpanBytes(),
		HopBytes:                  luagen.RelayHopBytes(),
		WidestSpanBytes:           j.Widest.SpanBytes,
		WidestSpanFunction:        j.Widest.Func,
		Stations:                  j.Stations,
		WidestSpanAfterRelayBytes: j.AfterBytes,
		WidestBlockBytes:          j.Blockiest.BlockBytes,
		WidestBlockFunction:       j.Blockiest.Func,
		BlockRoomBytes:            j.Blockiest.RoomBytes,
		Relayed:                   j.Stations > 0,
		RelayedFunctions:          []reportJumpFunc{},
	}
	for _, f := range j.Relayed {
		e.RelayedFunctions = append(e.RelayedFunctions, reportJumpFunc{
			Function:                  f.Func,
			WidestSpanBytes:           f.SpanBytes,
			Stations:                  f.Stations,
			WidestSpanAfterRelayBytes: f.AfterBytes,
			WidestBlockBytes:          f.BlockBytes,
			BlockRoomBytes:            f.RoomBytes,
		})
	}
	return e
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
	// A module that is not in this packaging still gets an entry, carrying the
	// two constants and zeros: an absent module is not a module with no jump,
	// and a consumer computing a percentage needs the denominator either way.
	r.Jumps.Control = jumpEntry(luagen.ModuleJumps{})
	r.Jumps.Data = jumpEntry(luagen.ModuleJumps{})
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
