package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/ir"
)

// The `api` command: pull, list and diff.
//
// Version handling is a first-class feature rather than a chore. Factorio ships
// breaking API changes on its own cadence and `latest` is routinely ahead of a
// typical install -- 2.1.12 against 2.0.77 while this was written -- so "will
// my mod survive the upgrade" is a question a mod author has, and `api diff`
// is what answers it without building anything.

const apiEndpoint = "https://lua-api.factorio.com/%s/runtime-api.json"

func runAPI(args []string) error {
	if len(args) == 0 {
		// ALL FOUR, and `check` is the one a script actually calls: it was
		// missing here while the unknown-subcommand line one return below named
		// it, so the two disagreed about what this command can do.
		return fmt.Errorf("usage: fklua api pull|list|diff|check")
	}
	switch args[0] {
	case "pull":
		return runAPIPull(args[1:])
	case "list":
		return runAPIList(args[1:])
	case "diff":
		return runAPIDiff(args[1:])
	case "check":
		return runAPICheck(args[1:])
	}
	return fmt.Errorf("unknown api subcommand %q (want pull, list, diff or check)", args[0])
}

// runAPIPull fetches one version's description into api/<version>/.
//
// Committed once fetched, which is why this is a separate command rather than
// something a build does: a build must never reach the network, and CI must
// never depend on lua-api.factorio.com being up.
func runAPIPull(args []string) error {
	var version string
	fromInstall := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--from-install":
			fromInstall = true
		case strings.HasPrefix(args[i], "-"):
			return fmt.Errorf("unknown flag %q", args[i])
		default:
			version = args[i]
		}
	}
	if version == "" && !fromInstall {
		return fmt.Errorf("usage: fklua api pull <version> | fklua api pull --from-install")
	}

	var body []byte
	var err error
	if fromInstall {
		body, err = readInstalledAPI()
		if err != nil {
			return err
		}
	} else {
		body, err = fetchAPI(version)
		if err != nil {
			return err
		}
	}

	// The version comes from the FILE, not from the argument. `latest` is a
	// real endpoint and resolves to whatever shipped most recently, so trusting
	// the argument would file it under a directory named "latest" that silently
	// means something different next month.
	a, err := factorio.ParseAPI(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("what came back is not a runtime-api.json: %w", err)
	}
	dir := filepath.Join(apiDir(), a.ApplicationVersion)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dir, "runtime-api.json")
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (api_version %d, %d classes, %d members, %d events)\n",
		dst, a.APIVersion, len(a.Classes), a.Members(), len(a.Events))
	if version != "" && version != a.ApplicationVersion {
		fmt.Printf("note: %q resolved to %s\n", version, a.ApplicationVersion)
	}
	return nil
}

func fetchAPI(version string) ([]byte, error) {
	url := fmt.Sprintf(apiEndpoint, version)
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// readInstalledAPI reads the description shipped with the local game, which is
// the authoritative one for whatever version is actually installed.
// The path is installedAPIPath()'s, in one place, because `fklua doctor` reads
// the same file to report the installed game's version and two spellings of one
// path drift the way every other mirror in this repo has.
func readInstalledAPI() ([]byte, error) {
	p := installedAPIPath()
	if p == "" {
		return nil, fmt.Errorf("no home directory, so the installed game cannot " +
			"be located (set FACTORIO_API_JSON to point at its runtime-api.json)")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("reading the installed API description: %w "+
			"(set FACTORIO_API_JSON to point at it)", err)
	}
	return b, nil
}

func runAPIList(args []string) error {
	current := false
	for _, a := range args {
		switch a {
		case "--current":
			current = true
		default:
			return fmt.Errorf("unknown argument %q (api list takes --current or nothing)", a)
		}
	}

	// --current is the MACHINE-READABLE half, and it exists because the table
	// below is not one however much it looks like one. The weekly api-regen bot
	// read the pin out of that table with `awk '/^\*/ {print $2}'`, which
	// matches the starred row AND the legend line under it -- so it wrote two
	// lines into $GITHUB_OUTPUT and every scheduled run since the legend was
	// added died on `Invalid format 'is'`. The legend was a presentation change
	// and could not have known it was editing a data interface.
	//
	// So a caller wanting one fact gets one line with nothing around it, and the
	// table stays free to grow a column or a footnote.
	if current {
		fmt.Println(factorio.DefaultAPIVersion)
		return nil
	}

	entries, err := os.ReadDir(apiDir())
	if err != nil {
		return fmt.Errorf("reading %s: %w", apiDir(), err)
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	sort.Strings(versions)
	if len(versions) == 0 {
		fmt.Println("no API descriptions cached; run `fklua api pull <version>`")
		return nil
	}
	for _, v := range versions {
		a, err := factorio.LoadAPI(apiPath(v))
		if err != nil {
			fmt.Printf("%-10s (unreadable: %v)\n", v, err)
			continue
		}
		mark := " "
		if v == factorio.DefaultAPIVersion {
			mark = "*" // the one the bindings are generated from
		}
		fmt.Printf("%s %-10s api_version %d  %d classes  %d members  %d events\n",
			mark, v, a.APIVersion, len(a.Classes), a.Members(), len(a.Events))
	}
	fmt.Println("\n* is the version the committed bindings are generated from")
	return nil
}

func runAPIDiff(args []string) error {
	var from, to, jsonPath string
	breakingOnly := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--breaking":
			breakingOnly = true
		case args[i] == "--json":
			if i+1 >= len(args) {
				return fmt.Errorf("--json needs a path")
			}
			i++
			jsonPath = args[i]
		case strings.HasPrefix(args[i], "-"):
			return fmt.Errorf("unknown flag %q", args[i])
		case from == "":
			from = args[i]
		case to == "":
			to = args[i]
		default:
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	if from == "" || to == "" {
		return fmt.Errorf("usage: fklua api diff <from> <to> [--breaking] [--json PATH]")
	}

	a, err := factorio.LoadAPI(apiPath(from))
	if err != nil {
		return fmt.Errorf("%s: %w (run `fklua api pull %s`)", from, err, from)
	}
	b, err := factorio.LoadAPI(apiPath(to))
	if err != nil {
		return fmt.Errorf("%s: %w (run `fklua api pull %s`)", to, err, to)
	}

	d := factorio.DiffAPI(a, b)
	if jsonPath != "" {
		raw, err := d.JSON()
		if err != nil {
			return err
		}
		if err := os.WriteFile(jsonPath, raw, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", jsonPath)
	}
	if breakingOnly {
		br := d.Breaking()
		for _, c := range br {
			fmt.Printf("BREAKING  %s — %s\n", c.What, c.Detail)
		}
		fmt.Printf("\n%d breaking change(s) from %s to %s\n", len(br), d.From, d.To)
		// A non-zero exit so a CI job can gate on it without parsing anything.
		if len(br) > 0 {
			os.Exit(1)
		}
		return nil
	}
	fmt.Print(d.Markdown())
	return nil
}

// runAPICheck answers "does MY mod survive the upgrade" for one compiled guest.
//
// THE EXIT CODE IS A CONTRACT, because the callers this feature exists for are
// scripts. A build harness deciding per guest whether a cross-series package is
// safe reads a status, not a paragraph:
//
//	0  nothing this guest uses breaks between the two versions
//	1  something does, OR the scan could not see everything the guest reaches
//	2  the check could not be run at all -- a bad flag, an unreadable module,
//	   a version this installation does not have
//
// 1 and 2 were one code until 2026-08-25, which made "your mod is fine and the
// pin move is safe" indistinguishable from "you typed the version wrong" to
// everything except a human reading stderr. 0 and 1 keep exactly the meanings
// they had, so a CI job gating on non-zero is unaffected.
//
// `--json` is the DATA INTERFACE and the table above it is not, however much a
// table looks like one -- this repo has already paid for that confusion once,
// in a weekly bot that read `api list`'s legend line as a version. The human
// report is unchanged and prints in exactly the cases it always did.
func runAPICheck(args []string) error {
	res, asJSON, err := checkGuestFromArgs(args)
	if err != nil {
		// Operational: nothing was checked, so there is no verdict to report.
		return &exitError{code: 2, msg: err.Error()}
	}
	if asJSON {
		raw, err := res.JSON()
		if err != nil {
			return &exitError{code: 2, msg: err.Error()}
		}
		os.Stdout.Write(raw)
	} else {
		fmt.Print(res.Report())
	}
	if code := res.ExitCode(); code != 0 {
		return &exitError{code: code}
	}
	return nil
}

// checkGuestFromArgs is everything that can fail operationally, separated so
// that the exit code above is decided in one place rather than at each return.
func checkGuestFromArgs(args []string) (factorio.CheckResult, bool, error) {
	var res factorio.CheckResult
	var wasmPath, to string
	asJSON := false
	// WHETHER --from WAS TYPED, rather than what it defaults to. The default is
	// no longer one constant -- it is checkFrom's four rules -- so "the caller
	// asked for a version" has to be a different question from "the value equals
	// DefaultAPIVersion", which is a version a caller can legitimately type. The
	// same shape --gc and --factorio-version carry in `fklua mod`.
	//
	// AND IT IS A BOOL RATHER THAN AN EMPTY-STRING SENTINEL, because the empty
	// string is a value a caller can PASS: `--from "$PIN"` with PIN unset arrives
	// here as a typed flag carrying nothing, and a sentinel would read that as
	// "no flag" and answer from the stamp -- silently, and with `from_source`
	// saying `stamp` to a caller that believes it named a version. That is the
	// filed defect's own shape one level down.
	fromFlag := ""
	fromTyped := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--to":
			if i+1 >= len(args) {
				return res, asJSON, fmt.Errorf("--to needs a version")
			}
			i++
			to = args[i]
		case args[i] == "--from":
			if i+1 >= len(args) {
				return res, asJSON, fmt.Errorf("--from needs a version")
			}
			i++
			if args[i] == "" {
				// THE SAME REFUSAL AS NO VALUE AT ALL, and the same words,
				// because it is the same mistake: an unexpanded shell variable
				// is a flag whose version went missing, not a request for the
				// resolver's other rules.
				return res, asJSON, fmt.Errorf("--from needs a version")
			}
			fromFlag = args[i]
			fromTyped = true
		case args[i] == "--json":
			// A BOOLEAN, WHERE `api diff --json` TAKES A PATH, and the
			// divergence is deliberate: this one's whole job is to be captured
			// by the shell that ran it, and a caller made to name a temp file to
			// read one verdict has been given a worse interface for symmetry's
			// sake. The cost is that `--json out.json` is a thing somebody will
			// type out of habit, which is why the positional below is refused
			// twice rather than silently overwritten -- the E1 shape (one flag
			// spelled two ways across two commands) is a trap only when getting
			// it wrong is quiet.
			asJSON = true
		case strings.HasPrefix(args[i], "-"):
			return res, asJSON, fmt.Errorf("unknown flag %q", args[i])
		default:
			if wasmPath != "" {
				return res, asJSON, fmt.Errorf(
					"expected one guest module, got %q and %q (note that "+
						"`api check --json` takes no path: the verdict goes to "+
						"stdout, unlike `api diff --json PATH`)", wasmPath, args[i])
			}
			wasmPath = args[i]
		}
	}
	if wasmPath == "" || to == "" {
		return res, asJSON, fmt.Errorf(
			"usage: fklua api check GUEST.wasm --to <version> [--from <version>] [--json]")
	}

	im, err := loadModule(wasmPath)
	if err != nil {
		return res, asJSON, err
	}
	from, fromSource, err := checkFrom(im, fromFlag, fromTyped)
	if err != nil {
		return res, asJSON, err
	}
	a, err := factorio.LoadAPI(apiPath(from))
	if err != nil {
		return res, asJSON, fmt.Errorf("%s: %w (run `fklua api pull %s`)", from, err, from)
	}
	b, err := factorio.LoadAPI(apiPath(to))
	if err != nil {
		return res, asJSON, fmt.Errorf("%s: %w (run `fklua api pull %s`)", to, err, to)
	}

	// All three tables come from `a`, the FROM description: member, event and
	// define ids are dense per-version indices, and the guest baked in the ones
	// its own bindings were generated against.
	report := factorio.GenerateMembers(a)
	evs := factorio.GenerateEvents(a)
	defs := factorio.GenerateDefines(a)
	usedM, mOK := factorio.UsedMembers(im)
	usedE, eOK := factorio.UsedEvents(im)
	usedD, dOK := factorio.UsedDefines(im)
	surface := factorio.SurfaceOf(report, usedM, mOK, usedE, eOK, evs, usedD, dOK, defs)

	// From and To come out of the DESCRIPTIONS' own application_version rather
	// than from what was typed, which is what makes echoing them worth anything:
	// a harness that passed no `--from` has no other way to learn which of
	// checkFrom's four rules answered, and `from_source` is the other half of
	// that -- the four resolve to the same string often enough that the version
	// alone does not say which question was asked.
	res = factorio.CheckGuest(surface, factorio.DiffAPI(a, b))
	res.Guest = wasmPath
	res.FromSource = fromSource
	return res, asJSON, nil
}

// checkFrom decides WHICH DESCRIPTION THE GUEST'S IDS ARE INDICES INTO, which is
// the only one this check may decode them against.
//
// THE DEFECT, filed by WormholeBelts as its FKLUA-GAPS item 9. Member, event and
// define ids are DENSE PER-DESCRIPTION INDICES, so a surface recovered from a
// module means something only against the description its bindings were
// generated from -- and `--from` defaulted to this binary's DefaultAPIVersion,
// which is a fact about the FKLUA BINARY and about nothing else. Measured on a
// guest stamped 2.1.17 that calls member 4500: `api check g.wasm --to 2.1.17`
// reported from 2.0.77, a surface of ZERO members, verdict clean, exit 0 -- a
// guest that calls a member told it touches nothing -- while `--from 2.1.17`
// reports the one member it really calls. The other direction is as quiet and
// worse: the same module read from 2.0.77 invented a removal of
// `LuaEntity::mining_progress`, a member that guest has never called.
//
// FOUR RULES, MOST-KNOWING FIRST. The flag is what the caller asked for. THE
// STAMP IS THE FACT -- a generated binding set exports fk_api_pin_<version> to
// name the description its ids were assigned over, which is this question
// exactly, and it is the half that was sitting in the module unread. The
// manifest is the project's stated INTENT, for a guest carrying no stamp. The
// default is the last resort and is unchanged, which is the whole of the
// compatibility: an unstamped guest with no manifest resolves as it always did.
//
// AND FOUR REFUSALS, every one of them exit 2 -- "the check could not be RUN"
// rather than a verdict -- because each leaves no description the ids could
// honestly be decoded against. Answering anyway is the defect above: not a wrong
// number, a fiction that reads exactly like an answer. Three are about the
// module (a --from contradicting its stamp, two stamps, a stamp naming a
// version this checkout has no description for); the fourth is about the rule
// that would have answered instead -- a manifest that is there and unreadable.
// `typed` IS THE TEST FOR RULE ONE, not `flag != ""`. The empty string is a
// version a caller can type, and the parse loop above refuses it there so that
// nothing here has to treat one as the other; taking the bool means this
// function is right whatever a future caller hands it.
func checkFrom(im *ir.Module, flag string, typed bool) (version, source string, err error) {
	pins := factorio.GuestPins(im)

	// WHY a wrong description is worse than no answer, shared by all three
	// refusals rather than written three times with a word different --
	// checkAPIPin's own arrangement, one command over.
	const why = "Member, event and define ids are DENSE SORTED INDICES over one " +
		"description's\nset, so a member added or removed anywhere shifts every " +
		"later id. Decoding this\nguest against another description names " +
		"DIFFERENT members -- silently, since every id\nstill resolves to " +
		"something -- so the surface, and the verdict over it, would be\na " +
		"fiction rather than a wrong number."

	if len(pins) > 1 {
		// REFUSED WHETHER OR NOT --from WAS TYPED, because no version can rescue
		// it: the two sets disagree about what every id past their first
		// difference means, so at most one of them is decoded correctly whatever
		// description is named. `fklua mod` refuses the same shape.
		var named []string
		exact := true
		for _, p := range pins {
			v, committed := pinStampVersion(p)
			named = append(named, v)
			exact = exact && committed
		}
		return "", "", fmt.Errorf(
			"this guest links %d generated binding sets, at APIs %s.\n"+
				"  what the module says: it exports %s\n"+
				"%s\n"+
				"A guest may link exactly ONE, so there is no description this "+
				"check could decode\nit against. Regenerate every binding set it "+
				"imports at ONE pin: `fklua gen-bindings`\nfor the project's own, "+
				"and `fklua gen-bindings --into DIR` for the committed copy\ninside "+
				"a vendored FkLua checkout. Then rebuild the guest.%s",
			len(pins), strings.Join(named, " and "), strings.Join(pins, " and "), why,
			guessCaveat(exact, false))
	}

	if len(pins) == 1 {
		stamp := pins[0]
		// WHICH VERSION THIS STAMPED GUEST RESOLVES TO, and which rule answered
		// with it, decided BEFORE the notice below. WHETHER the notice fires is a
		// function of (stamp, manifest) and not of which rule fired -- see
		// noteStampManifestDivergence -- so both arms reach it and only one of
		// this function's own refusals, which resolve no version at all,
		// pre-empts it. WHAT it says about the rule is `source`, handed over
		// rather than assumed, because both arms get here.
		version, source := "", factorio.FromSourceStamp
		if typed {
			// COMPARED AS MANGLED NAMES rather than by recovering the stamp's
			// version, so this holds for a description this checkout does not
			// carry as well as for one it does. PinExport is what both
			// generators spell the export with, so the comparison is exact.
			if factorio.PinExport(flag) != stamp {
				named, committed := pinStampVersion(stamp)
				return "", "", fmt.Errorf(
					"this guest was built against API %s bindings, and --from names "+
						"API %s.\n"+
						"  what asked for %s: the --from flag\n"+
						"  what the module says: it exports %s, which every generated "+
						"binding set carries\n    to name the description its ids were "+
						"assigned over\n"+
						"%s\n"+
						"Two ways to reconcile:\n"+
						"  (1) DROP --from. With no flag it resolves to the guest's own "+
						"stamp, which is\n      the fact about what its ids are indices "+
						"into.\n"+
						"  (2) PASS THE STAMP'S VERSION: --from %s.%s",
					named, flag, flag, stamp, why, named, guessCaveat(committed, true))
			}
			version, source = flag, factorio.FromSourceFlag
		} else {
			v, ok := pinCommittedVersion(stamp)
			if !ok {
				// ALWAYS A READING rather than a fact, by construction: this is
				// the branch where no committed description carries the version,
				// which is the only thing that could have spelled it exactly.
				named, _ := pinStampVersion(stamp)
				return "", "", fmt.Errorf(
					"this guest was built against API %s bindings, and no description "+
						"for that\nversion is committed here.\n"+
						"  what the module says: it exports %s, which every generated "+
						"binding set carries\n    to name the description its ids were "+
						"assigned over\n"+
						"%s\n"+
						"Pull it and check again: `fklua api pull %s` (`fklua api list` "+
						"shows what this\ninstallation has).%s",
					named, stamp, why, named, guessCaveat(false, true))
			}
			version = v
		}
		noteStampManifestDivergence(stamp, version, source)
		return version, source, nil
	}

	// NO STAMP IS SILENCE, exactly as it is for `fklua mod`: bindings generated
	// before the stamp existed carry none, and a guest linking no generated
	// bindings carries none either. What answers then is what the caller said.
	if typed {
		return flag, factorio.FromSourceFlag, nil
	}
	proj, ok, err := loadProject()
	if err != nil {
		// A manifest that is present and unreadable is not a reason to fall
		// through to the default quietly: answering against a version nobody
		// chose is the exact shape of the defect this function exists to close.
		//
		// WRAPPED RATHER THAN RETURNED RAW, and that is the fourth refusal
		// rather than a rewording. loadProject's error is about a FILE -- e.g.
		// `fklua.toml: line 7: unknown key "some_future_key" in [fklua]`, which
		// is what an older fklua makes of a manifest a newer one wrote -- and it
		// names neither the question this command was asking nor the flag that
		// answers it without a manifest. A caller checking a guest is then
		// looking at an exit 2 about a file it never mentioned.
		return "", "", fmt.Errorf(
			"this guest carries no pin stamp, so the working directory's %s was "+
				"consulted\nfor `[fklua] api` -- and it could not be read.\n"+
				"  what the module says: nothing. It exports no %s* function, "+
				"which is what a\n    generated binding set carries to name the "+
				"description its ids were assigned over\n"+
				"  what the manifest says: %w\n"+
				"%s\n"+
				"Two ways forward:\n"+
				"  (1) PASS --from <version>. It answers without the manifest, and "+
				"an unstamped\n      guest is the case the flag exists for.\n"+
				"  (2) FIX %s, or run from a directory that has none.",
			projectFile, factorio.PinExportPrefix, err, why, projectFile)
	}
	if ok {
		// NO `&& proj.API != ""`, and its absence is deliberate rather than an
		// omission. ParseProject makes `[fklua] api` REQUIRED, so a manifest
		// that parsed HAS a pin: there is no "present but empty" state for the
		// guard to catch, and it would be dead today and worse than dead
		// tomorrow. If the key ever became optional, that guard would hand this
		// question to the default without a word -- the exact silent
		// fall-through this resolver exists to close -- where its absence makes
		// the empty pin an unreadable-description error that names itself.
		return proj.API, factorio.FromSourceManifest, nil
	}
	return factorio.DefaultAPIVersion, factorio.FromSourceDefault, nil
}

// noteStampManifestDivergence says so out loud when the guest's own stamp and
// the project's manifest name DIFFERENT descriptions.
//
// NOT A REFUSAL, and the distinction is the whole of why this is a notice.
// `api check` answers a question about a GUEST -- which description are this
// module's ids indices into -- and the stamp is the fact that answers it, so
// proceeding from the stamp is right even when the project meant something else.
// What is NOT right is answering silently: `fklua mod` REFUSES this exact
// pairing rather than choosing (checkAPIPin), because a packaged table whose ids
// were assigned over a different description answers the guest's calls with
// different members, silently wherever the kinds line up, in a lockstep game. A
// caller who reads a clean verdict here and then packages is walking into that
// refusal, and a caller who reads it as covering the build the PROJECT would
// make has read it wrong.
//
// A FUNCTION OF (STAMP, MANIFEST), AND OF NOTHING ELSE -- which is why checkFrom
// calls it once, after the stamp has resolved, rather than on the untyped arm
// alone. The refusal it warns about (`checkAPIPin`) reads the stamp and the
// packaging pin and has never heard of `--from`, so the arrangement is exactly
// as present when the caller typed `--from` naming the stamp's own version --
// which is the shape a downstream harness types, as a same-pin gate -- as when
// it typed nothing. Firing on one and not the other left the arm every harness
// reaches silent.
//
// STDERR, because stdout carries the verdict a script parses -- `--json`'s whole
// contract is that nothing else lands there.
//
// IT TAKES THE RESOLVED SOURCE AND PRINTS FromSourcePhrase RATHER THAN SPELLING
// A RULE, for the reason that function is one function: this notice is a THIRD
// output describing which of the four answered, beside the report's header and
// the document's `from_source`, and a sentence that hard-codes one of them is
// wrong on the other arm -- two lines above a header that says otherwise. It
// also PROMISES NO VERDICT. checkFrom calls this before either description is
// loaded, so a run that refuses downstream (an unreadable `--to`, say) prints
// the notice and then nothing else; "the verdict below" was a claim about output
// this function cannot know will exist.
func noteStampManifestDivergence(stamp, stamped, source string) {
	proj, ok, err := loadProject()
	// An unreadable manifest is not this function's business: the stamp already
	// answered, and the refusal above covers the case where the manifest is the
	// rule that would have.
	if err != nil || !ok || proj.API == stamped {
		return
	}
	fmt.Fprintf(os.Stderr,
		"NOTICE: this guest is stamped API %s and %s pins API %s.\n"+
			"  what the module says: it exports %s, which every generated binding "+
			"set carries\n    to name the description its ids were assigned over\n"+
			"  what the project says: api = %q in %s\n"+
			"This check resolved API %s, which this guest's stamp names, so what it\n"+
			"reports is about the guest as it was BUILT rather than as the project\n"+
			"would package it. The rule that answered: %s.\n"+
			"`fklua mod` refuses this pairing rather than choosing between them, so "+
			"reconcile\nthem before packaging: regenerate the bindings this guest "+
			"imports at %s and\nrebuild it (`fklua gen-bindings`, and "+
			"`fklua gen-bindings --into DIR` for the\ncommitted copy inside a "+
			"vendored FkLua checkout), or move the project's pin to %s.\n",
		stamped, projectFile, proj.API, stamp, proj.API, projectFile,
		stamped, factorio.FromSourcePhrase(source),
		proj.API, stamped)
}

// pinStampVersion is the version to NAME in a refusal about a stamp, and
// whether that spelling is a FACT or a READING.
//
// The committed directory's own name when this checkout has one, which is
// exact, and `committed` is true. Otherwise the stamp read back: PinExport
// writes every character outside [0-9A-Za-z] as '_' and has no inverse, so
// putting the dots back is a guess -- an exact one for every Factorio version
// there has ever been, since those are digits and dots, but a guess. The second
// return is what makes the difference SAYABLE: every refusal that prints a
// recovered version appends guessCaveat, so none of them implies a spelling it
// cannot know. Returning the bool rather than leaving each caller to re-ask
// pinCommittedVersion is what keeps the two halves from drifting apart.
func pinStampVersion(stamp string) (version string, committed bool) {
	if v, ok := pinCommittedVersion(stamp); ok {
		return v, true
	}
	return strings.ReplaceAll(
		strings.TrimPrefix(stamp, factorio.PinExportPrefix), "_", "."), false
}

// guessCaveat is the sentence a refusal adds when it printed a version it read
// back out of an export name, and nothing when it printed one this checkout
// carries a description for.
//
// ONE NAMING HALF for all three refusals, for the reason `why` is one: the
// caveat has to say the same thing wherever it lands, and a refusal that names
// an exact version must not carry it -- "this might not be how it is spelled"
// about a version read off a committed directory is noise that teaches a reader
// to skip the paragraph.
//
// AND A SEPARATE ACTIONABLE TAIL, because "pass the real spelling as --from" is
// only an instruction where `--from` can change the outcome. It cannot for the
// TWO-STAMP refusal: `len(pins) > 1` returns before the flag is ever read, and
// the refusal's own body two lines above says a guest may link exactly ONE and
// there is no description this check could decode it against. A message that
// contradicts itself spends the reader's next attempt on a run that prints the
// same words. The naming half still belongs there -- it is what stops a reader
// grepping for a directory spelled `9.9.9` -- so the two are separate rather
// than the caveat being dropped.
func guessCaveat(committed, fromCanAnswer bool) string {
	if committed {
		return ""
	}
	// "A VERSION NAMED ABOVE" rather than "the version above", because the
	// two-stamp refusal names two and only one of them may be a reading.
	s := "\nA VERSION NAMED ABOVE IS READ BACK OUT OF ITS EXPORT NAME, not " +
		"looked up: a stamp\nwrites every character outside [0-9A-Za-z] as `_` " +
		"and the mangling has no inverse,\nso `_` stands for whatever separator " +
		"the real version uses. That reading is exact\nfor a version spelled in " +
		"digits and dots, which every Factorio release has been."
	if fromCanAnswer {
		s += "\nIf this one is spelled some other way, pass the real spelling " +
			"as --from."
	}
	return s
}

// runDocs renders the per-language API reference.
func runDocs(args []string) error {
	lang, out := "go", "docs"
	version := factorio.DefaultAPIVersion
	for i := 0; i < len(args); i++ {
		switch {
		case isLangArg(args[i]):
			// Both spellings, for the reason in langArg: this command took a
			// space and gen-bindings took an equals, for the same flag.
			v, next, err := langArg(args, i)
			if err != nil {
				return err
			}
			lang, i = v, next
		case args[i] == "--api":
			if i+1 >= len(args) {
				return fmt.Errorf("--api needs a version")
			}
			i++
			version = args[i]
		case args[i] == "-o":
			if i+1 >= len(args) {
				return fmt.Errorf("-o needs a path")
			}
			i++
			out = args[i]
		default:
			return fmt.Errorf("unknown argument %q", args[i])
		}
	}
	if lang != "go" && lang != "rust" {
		// `all` is spelled out because gen-bindings takes it and this does not,
		// and the two commands are one flag apart in the usage text.
		return fmt.Errorf("--lang must be go or rust, not %q: docs renders one "+
			"language per run (unlike gen-bindings, there is no `all`) -- run it "+
			"twice into the same -o directory for both", lang)
	}

	a, err := factorio.LoadAPI(apiPath(version))
	if err != nil {
		return fmt.Errorf("%s: %w (run `fklua api pull %s`)", version, err, version)
	}
	report := factorio.GenerateMembers(a)
	evs := factorio.GenerateEvents(a)

	// The names come from the SAME generator run that produces the bindings, so
	// the docs cannot name a member something the bindings do not.
	names := map[string]string{}
	if lang == "go" {
		g, err := factorio.GenerateGoWith(a, report, evs, "fkapi")
		if err != nil {
			return err
		}
		names = g.Names
	} else {
		r, err := factorio.GenerateRust(a, report, evs)
		if err != nil {
			return err
		}
		names = r.Names
	}

	md := factorio.Docs(a, report, evs, factorio.DocOptions{Lang: lang, Names: names})
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(out, "api-"+lang+".md")
	if err := os.WriteFile(dst, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d members, %d bytes)\n", dst, len(names), len(md))
	return nil
}
