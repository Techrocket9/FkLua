// Package guest describes the WebAssembly feature surface FkLua can compile,
// and reads back what the guest toolchains actually emit.
//
// The point is to make a fragile assumption fail loudly. Which wasm proposals
// we need to support is not a property of the spec -- it is a property of
// whatever TinyGo and Rust happen to enable by default, and that moves between
// releases. The corpus in scripts/fetch-spec.sh hardcodes one such feature
// string in a comment, which is documentation and can rot silently.
//
// This has already bitten once: i32/i64.trunc_sat_* went unimplemented for
// three milestones because "TinyGo emits nontrapping-fptoint unconditionally"
// was recorded in prose and never checked.
package guest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Feature is a WebAssembly proposal as named in an LLVM/TinyGo feature string.
type Feature string

// Supported lists every feature FkLua can compile today.
//
// A guest toolchain enabling anything outside this set produces modules we will
// refuse, so the set is the real contract -- not the exact feature string, which
// can gain neutral entries or change order without meaning anything.
var Supported = map[Feature]bool{
	"sign-ext":               true, // i32/i64.extend8_s and friends
	"nontrapping-fptoint":    true, // trunc_sat
	"mutable-globals":        true,
	"call-indirect-overlong": true, // an encoding detail the decoder absorbs
}

// Planned maps a feature we do NOT support onto a note saying what its status
// actually is. Anything here is known rather than a surprise.
//
// bulk-memory is PARTIAL and saying so is the point. memory.copy and memory.fill
// -- the two a guest toolchain actually emits -- are compiled, and natively, at
// 3.5 and 2.2 ns/byte against 173 for the byte loop binaryen would lower them
// into. The segment-indexed half is not: memory.init, data.drop, table.copy,
// table.init and elem.drop need the data and elem sections kept live past
// instantiation, which is a different change and is not scheduled.
//
// It read "M10" until the audit, two milestones after M10 shipped, which turns
// a decision into a roadmap item nobody is working on. If a guest is ever
// observed emitting memory.init, THAT is when to schedule the rest.
var Planned = map[Feature]string{
	"bulk-memory":     "partial: memory.copy/fill compiled, segment-indexed ops unscheduled",
	"bulk-memory-opt": "partial: memory.copy/fill compiled, segment-indexed ops unscheduled",
}

// Target is one guest toolchain configuration we care about.
type Target struct {
	// Name is the toolchain's own target name.
	Name string
	// MustBeFullySupported is true for targets a milestone already claims to
	// compile. A planned-but-missing feature is a failure for those.
	MustBeFullySupported bool
	// Why records what the target is for, so a failure explains itself.
	Why string
}

// TinyGoTargets are the configurations the roadmap commits to.
var TinyGoTargets = []Target{
	{Name: "wasm-unknown", MustBeFullySupported: true,
		Why: "the M4 flagship guest target"},
	{Name: "wasip1", MustBeFullySupported: false,
		Why: "the M10 target; bulk-memory is a known, scheduled gap"},
}

// tinygoTarget is the subset of a TinyGo target JSON we read.
type tinygoTarget struct {
	Inherits []string `json:"inherits"`
	Features string   `json:"features"`
}

// Root returns TinyGo's install root, or an error if TinyGo is absent.
func Root() (string, error) {
	out, err := exec.Command("tinygo", "env", "TINYGOROOT").Output()
	if err != nil {
		return "", fmt.Errorf("tinygo not available: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Features reads a TinyGo target's enabled and disabled features, following
// `inherits` until a target declares its own feature string.
func Features(root, target string) (enabled, disabled []Feature, err error) {
	seen := map[string]bool{}
	name := target
	for i := 0; i < 16; i++ {
		if seen[name] {
			return nil, nil, fmt.Errorf("target %q: inherits cycle", target)
		}
		seen[name] = true

		raw, err := os.ReadFile(filepath.Join(root, "targets", name+".json"))
		if err != nil {
			return nil, nil, err
		}
		var t tinygoTarget
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, nil, fmt.Errorf("target %q: %w", name, err)
		}
		if t.Features != "" {
			en, dis := parseFeatures(t.Features)
			return en, dis, nil
		}
		if len(t.Inherits) == 0 {
			return nil, nil, fmt.Errorf("target %q: no feature string and nothing to inherit", target)
		}
		name = t.Inherits[0]
	}
	return nil, nil, fmt.Errorf("target %q: inherits chain too deep", target)
}

// parseFeatures splits an LLVM feature string such as "+sign-ext,-multivalue".
func parseFeatures(s string) (enabled, disabled []Feature) {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if len(part) < 2 {
			continue
		}
		f := Feature(part[1:])
		switch part[0] {
		case '+':
			enabled = append(enabled, f)
		case '-':
			disabled = append(disabled, f)
		}
	}
	sort.Slice(enabled, func(i, j int) bool { return enabled[i] < enabled[j] })
	sort.Slice(disabled, func(i, j int) bool { return disabled[i] < disabled[j] })
	return
}

// Gap is a feature a guest emits that FkLua cannot compile.
type Gap struct {
	Feature Feature
	// Milestone is when it is scheduled, or "" if it is not on the roadmap at
	// all -- which means a toolchain has started emitting something new.
	Milestone string
}

// Check reports every enabled feature FkLua does not support.
func Check(enabled []Feature) []Gap {
	var gaps []Gap
	for _, f := range enabled {
		if Supported[f] {
			continue
		}
		gaps = append(gaps, Gap{Feature: f, Milestone: Planned[f]})
	}
	return gaps
}
