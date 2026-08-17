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
		return fmt.Errorf("usage: fklua api pull|list|diff")
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
func runAPICheck(args []string) error {
	var wasmPath, to string
	from := factorio.DefaultAPIVersion
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--to":
			if i+1 >= len(args) {
				return fmt.Errorf("--to needs a version")
			}
			i++
			to = args[i]
		case args[i] == "--from":
			if i+1 >= len(args) {
				return fmt.Errorf("--from needs a version")
			}
			i++
			from = args[i]
		case strings.HasPrefix(args[i], "-"):
			return fmt.Errorf("unknown flag %q", args[i])
		default:
			wasmPath = args[i]
		}
	}
	if wasmPath == "" || to == "" {
		return fmt.Errorf("usage: fklua api check GUEST.wasm --to <version> [--from <version>]")
	}

	im, err := loadModule(wasmPath)
	if err != nil {
		return err
	}
	a, err := factorio.LoadAPI(apiPath(from))
	if err != nil {
		return fmt.Errorf("%s: %w (run `fklua api pull %s`)", from, err, from)
	}
	b, err := factorio.LoadAPI(apiPath(to))
	if err != nil {
		return fmt.Errorf("%s: %w (run `fklua api pull %s`)", to, err, to)
	}

	report := factorio.GenerateMembers(a)
	evs := factorio.GenerateEvents(a)
	usedM, mOK := factorio.UsedMembers(im)
	usedE, eOK := factorio.UsedEvents(im)
	surface := factorio.SurfaceOf(report, usedM, mOK, usedE, eOK, evs)

	res := factorio.CheckGuest(surface, factorio.DiffAPI(a, b))
	fmt.Print(res.Report())
	// Non-zero when something the guest uses breaks, so CI can gate without
	// parsing. An incomplete scan is also non-zero: unproven is not a pass.
	if len(res.Hits) > 0 || !surface.Complete {
		os.Exit(1)
	}
	return nil
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
