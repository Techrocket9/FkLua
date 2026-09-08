package main

import (
	"fmt"
	"os"

	"github.com/Techrocket9/fklua/internal/modsettings"
)

// The `modsettings` command: write a mod-settings.dat from JSON, and read one
// back out as JSON.
//
// WHY A COMPILER OWNS THIS COMMAND. A mod's settings stage is a guest now, and
// the strongest thing that can be said about a settings stage is what the
// ENGINE does with a value a player chose. That value reaches the stages
// through mod-settings.dat, which the engine writes and reads back and which
// nothing else writes: there is no CLI for it, no text form and no flag. So an
// automated check of the other side of a setting has to write those bytes, and
// putting the writer HERE rather than in each project means every mod and every
// guest library on the channel gets that check by writing its own JSON.
//
// IT IS ALSO WHY THE IN-GAME GATE CALLS THIS BINARY. scripts/run-datastage.sh
// builds bin/fklua every run anyway, and a second tool for this would be a
// second thing to build, a second thing to keep in step with the format, and a
// second answer when the two disagreed.

func runModSettings(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fklua modsettings write --from FILE.json --out mod-settings.dat\n" +
			"       fklua modsettings read mod-settings.dat")
	}
	switch args[0] {
	case "write":
		return runModSettingsWrite(args[1:])
	case "read":
		return runModSettingsRead(args[1:])
	}
	return fmt.Errorf("unknown modsettings subcommand %q (want write or read)", args[0])
}

// runModSettingsWrite turns the JSON description into the file the engine
// reads.
func runModSettingsWrite(args []string) error {
	from, out := "", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--from":
			if i+1 >= len(args) {
				return fmt.Errorf("--from needs a path")
			}
			i++
			from = args[i]
		case "--out":
			if i+1 >= len(args) {
				return fmt.Errorf("--out needs a path")
			}
			i++
			out = args[i]
		default:
			return fmt.Errorf("unknown argument %q; write takes --from and --out and nothing else", args[i])
		}
	}
	if from == "" || out == "" {
		return fmt.Errorf("usage: fklua modsettings write --from FILE.json --out mod-settings.dat")
	}

	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	doc, err := modsettings.ParseJSON(data)
	if err != nil {
		return fmt.Errorf("%s: %w", from, err)
	}
	encoded := modsettings.Encode(doc)

	// READ BACK BEFORE WRITING. The engine's failure mode for a file it cannot
	// parse is a refusal that names the file and not the byte, and the gate
	// that runs this is a twenty-second engine run; a round trip here costs
	// microseconds and turns "Factorio would not start" into a sentence naming
	// the byte.
	if _, err := modsettings.Decode(encoded); err != nil {
		return fmt.Errorf("what was encoded from %s does not read back: %w", from, err)
	}

	// 0644 rather than 0600: the engine reads this as whatever user runs it,
	// and a private user directory is what keeps a gate out of an install.
	if err := os.WriteFile(out, encoded, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %d bytes to %s\n", len(encoded), out)
	return nil
}

// runModSettingsRead prints a mod-settings.dat in the JSON form `write` reads.
//
// STDOUT IS EXACTLY ONE DOCUMENT and nothing else, so a caller can pipe it into
// jq or hand it straight back to `write`.
func runModSettingsRead(args []string) error {
	path := ""
	for i := 0; i < len(args); i++ {
		if len(args[i]) > 0 && args[i][0] == '-' {
			return fmt.Errorf("unknown flag %q; read takes one path and no flags", args[i])
		}
		if path != "" {
			return fmt.Errorf("read takes one mod-settings.dat, and it was given %s and %s", path, args[i])
		}
		path = args[i]
	}
	if path == "" {
		return fmt.Errorf("usage: fklua modsettings read mod-settings.dat")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	doc, err := modsettings.Decode(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	text, err := modsettings.RenderJSON(doc)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	os.Stdout.Write(text)
	return nil
}
