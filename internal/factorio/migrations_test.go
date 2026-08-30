package factorio

import (
	"strings"
	"testing"
)

// THE COUNT OF HAND-WRITTEN LUA STAYS AUDITABLE.
//
// `migrations/*.lua` is not FkLua's state-migration mechanism and will not
// become one: a mod's own state is the guest's heap, and the heap is migrated by
// `fk_migrate`, never by a file the engine runs BEFORE the heap has been
// adopted. What the file type keeps is the status of inline assembly --
// permitted, marked, minimised, never generated -- and the packager's line is
// the mark, so a repository can grep its own build output rather than
// remembering to look.
//
// The measurement behind that doctrine: the base game has shipped exactly ONE
// Lua migration in its whole 1.1-to-2.0 history, the median migration across ten
// sampled mods is eleven lines, and none of them needed the two properties only
// the mechanism has.
func TestLuaMigrationsAreCountedAndJSONOnesAreNot(t *testing.T) {
	p := &Package{
		Info: Info{Name: "fk-mig", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: "-- chunk\n", APITable: emptyAPITable,
		Extra: map[string]string{
			"migrations/2020-01-01_fix.lua":     "-- theirs\n",
			"migrations/2019-06-02_rename.json": "{}\n",
			// A JSON migration is a prototype-rename TABLE rather than a program:
			// there is nothing there a compiler could have replaced, and reporting
			// one would be reporting an author for using the format correctly.
			"migrations/2018-01-01_early.lua": "-- theirs\n",
			// Not a migration at all, and named to look like one from a distance.
			"prototypes/migrations.lua": "-- data stage\n",
			"migrations/README.md":      "notes\n",
		},
	}
	got := p.LuaMigrations()
	want := []string{
		"migrations/2018-01-01_early.lua",
		"migrations/2020-01-01_fix.lua",
	}
	if len(got) != len(want) {
		t.Fatalf("counted %v, want %v", got, want)
	}
	// SORTED, because this reaches a build line and a line that reordered itself
	// between runs would make every rebuild a diff.
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] %q, want %q", i, got[i], want[i])
		}
	}
}

// A mod with no migrations reports nothing, which is what makes the line a MARK
// rather than a nag.
func TestAModWithNoLuaMigrationsCountsNone(t *testing.T) {
	p := &Package{
		Info: Info{Name: "fk-mig2", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: "-- chunk\n", APITable: emptyAPITable,
		Extra: map[string]string{"migrations/2020-01-01_rename.json": "{}\n"},
	}
	if got := p.LuaMigrations(); len(got) != 0 {
		t.Errorf("a mod whose only migration is JSON reported %v", got)
	}
}

// A migration is a FILE IN migrations/, not anything below it. Factorio reads
// that directory and not a tree under it, so a Lua file one level deeper is an
// author's own module and is not a migration the engine will ever run.
func TestANestedLuaFileIsNotAMigration(t *testing.T) {
	p := &Package{
		Info: Info{Name: "fk-mig3", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: "-- chunk\n", APITable: emptyAPITable,
		Extra: map[string]string{"migrations/helpers/util.lua": "-- helper\n"},
	}
	if got := p.LuaMigrations(); len(got) != 0 {
		t.Errorf("a file below migrations/ was counted as a migration: %v", got)
	}
}

// AND THE MIGRATIONS ARE STILL PACKAGED, which is the half that says this is a
// mark and not a filter. The doctrine permits them; it declines to generate one.
func TestALuaMigrationIsStillCarriedIntoThePackage(t *testing.T) {
	p := &Package{
		Info: Info{Name: "fk-mig4", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: "-- chunk\n", APITable: emptyAPITable,
		Extra: map[string]string{"migrations/2020-01-01_fix.lua": "-- theirs\n"},
	}
	files, err := p.Files()
	if err != nil {
		t.Fatal(err)
	}
	if body, ok := files["migrations/2020-01-01_fix.lua"]; !ok {
		t.Error("the migration was dropped from the package")
	} else if !strings.Contains(body, "theirs") {
		t.Errorf("the migration was rewritten: %q", body)
	}
}
