package factorio

import (
	"strings"
	"testing"
)

// THE OFFSET, DERIVED FROM THE WRAPPER ITSELF rather than remembered.
//
// A debug map's line ranges are measured in the emitted chunk and consumed in
// the packaged file, and the only thing between the two is this wrapper. An
// off-by-one does not fail anything else: the mod loads, every range is one
// line out, and every stack frame is attributed to whichever function happens
// to be next door.
//
// Red proof: set ChunkLineOffset to 4 and this reports a marker found on line 6
// against a claimed offset of 4.
func TestTheWrapperOffsets(t *testing.T) {
	const marker = "-- THE FIRST LINE OF THE CHUNK\n"
	for _, tc := range []struct {
		what   string
		wrap   func(string) string
		offset int
	}{
		{"wrapChunk", wrapChunk, ChunkLineOffset},
		{"wrapDataChunk", wrapDataChunk, DataChunkLineOffset},
	} {
		t.Run(tc.what, func(t *testing.T) {
			lines := strings.Split(tc.wrap(marker), "\n")
			at := -1
			for i, l := range lines {
				if strings.HasPrefix(l, "-- THE FIRST LINE") {
					at = i + 1 // 1-based, as a line number is
					break
				}
			}
			if at < 0 {
				t.Fatalf("the wrapper dropped the chunk:\n%s", strings.Join(lines, "\n"))
			}
			// The chunk's line 1 lands at line 1+offset in the packaged file.
			if got := at - 1; got != tc.offset {
				t.Errorf("%s prepends %d line(s), and the constant says %d",
					tc.what, got, tc.offset)
			}
		})
	}
}

// The map is written beside the module it describes, on the same gate. A
// package that carries no map ships exactly the files it always shipped, which
// is what makes --no-map a subtraction rather than a different packager.
func TestTheMapIsWrittenBesideItsModule(t *testing.T) {
	p := dataPackage("fk_data")
	p.MapJSON = `{"fklua_map":1}` + "\n"
	p.DataMapJSON = `{"fklua_map":1,"module":"fk_data_module.lua"}` + "\n"
	files, err := p.Files()
	if err != nil {
		t.Fatal(err)
	}
	if got := files[MapFile]; got != p.MapJSON {
		t.Errorf("%s = %q", MapFile, got)
	}
	if got := files[DataMapFile]; got != p.DataMapJSON {
		t.Errorf("%s = %q", DataMapFile, got)
	}

	// ...and without them, the same package ships neither file and nothing else
	// moves.
	bare := dataPackage("fk_data")
	before, err := bare.Files()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before[MapFile]; ok {
		t.Errorf("a package with no map shipped %s", MapFile)
	}
	if _, ok := before[DataMapFile]; ok {
		t.Errorf("a package with no data map shipped %s", DataMapFile)
	}
	if len(files) != len(before)+2 {
		t.Errorf("the maps changed the file list by %d entries, not 2:\n  %v\n  %v",
			len(files)-len(before), names(files), names(before))
	}
}

// A map cannot outlive the module it describes: a data-stage-only mod has no
// fk_module.lua, so a control map set on it would be a file of line numbers
// into nothing.
func TestAMapWithoutItsModuleIsNotShipped(t *testing.T) {
	p := dataPackage("fk_data")
	p.Chunk = ""
	p.MapJSON = `{"fklua_map":1}` + "\n"
	p.DataMapJSON = `{"fklua_map":1}` + "\n"
	files, err := p.Files()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[MapFile]; ok {
		t.Errorf("a mod with no %s shipped %s", GeneratedModuleFile, MapFile)
	}
	if _, ok := files[DataMapFile]; !ok {
		t.Errorf("a mod with a data module shipped no %s", DataMapFile)
	}
}
