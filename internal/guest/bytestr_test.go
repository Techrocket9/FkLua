package guest_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// A LUA STRING IS BYTES, AND BOTH GENERATED READERS HAND BACK THE SAME ONES.
//
// The Go generator's getStr has always been `string(b)`, which is byte-exact
// because a Go string is a byte string. The Rust one was
// `String::from_utf8_lossy(..).into_owned()`, because a Rust String is not --
// so for four milestones every generated Rust reader silently rewrote any
// engine string that was not valid UTF-8 into U+FFFD sequences AND changed its
// length, and the two backends disagreed about what one wire said. Nothing
// caught it because every string in this repo's own corpus is ASCII: the same
// shape as "the Rust generator was four milestones behind", found the same way,
// by a mod written outside this repo. `guest/rust/fkipc` carried a workaround
// that read the (pointer, length) pair itself rather than trust the reader.
//
// So this drives BOTH dict guests -- one host stub, one script, one set of
// expectations -- with all 256 byte values through two different generated
// readers, and asserts at the BYTES:
//
//   - `on_console_chat.message`, a mandatory STRING FIELD on an event payload,
//     decoded by the generated struct's own decode_at.
//   - a `tags` entry, which is a TIER-2 value, decoded by read_dyn -- a
//     different code path through the same wire and the one a mod's own data
//     travels.
//   - and the same bytes going straight back OUT through
//     `helpers.write_file`, printed by the host, so the outbound half is
//     pinned outside `fkipc` too. That direction was never lossy, but it is
//     where `Value::Str` owning a `String` forced a guest into
//     `from_utf8_unchecked` to say anything binary at all.
//
// Both are printed as `<len>:<hex>`, because the lossy reader got the LENGTH
// wrong as well as the contents and a test asserting only one of them could
// have passed. Every byte 0x00..0xFF is in there, so NUL does not truncate,
// 0xFF is not a replacement character, and a byte pair that happens to be a
// valid UTF-8 sequence is not re-encoded.
func TestABinaryStringCrossesAGeneratedEventReaderByteExact(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		if ok, why := guest.Available(); !ok {
			t.Skipf("skipping: %s", why)
		}
		tmp := t.TempDir()
		p := filepath.Join(tmp, "dict.wasm")
		if err := guest.Build(filepath.Join(repoRoot(t), "guest", "go"), "./examples/dict", p); err != nil {
			t.Fatalf("building the Go guest: %v", err)
		}
		checkBinaryStringGuest(t, p)
	})
	t.Run("rust", func(t *testing.T) {
		if ok, why := guest.RustAvailable(); !ok {
			t.Skipf("skipping: %s", why)
		}
		tmp := t.TempDir()
		p, err := guest.BuildRust(filepath.Join(repoRoot(t), "guest", "rust"), "dict",
			filepath.Join(tmp, "cargo"))
		if err != nil {
			t.Fatalf("building the Rust guest: %v", err)
		}
		checkBinaryStringGuest(t, p)
	})
}

func checkBinaryStringGuest(t *testing.T, wasmPath string) {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := packageDictGuest(t, wasmPath)

	// The expectation is DERIVED, not written down: 0x00 through 0xFF in order,
	// which is also what the Lua below builds. Writing the hex out by hand would
	// be 512 characters nobody could check.
	var hex strings.Builder
	for i := 0; i < 256; i++ {
		fmt.Fprintf(&hex, "%02x", i)
	}
	all := "256:" + hex.String()

	out, err := h.RunString(fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end
defines = { events = { on_built_entity = 42, on_console_chat = 24 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-dict",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
local ent = { valid = true, name = "iron-chest", object_name = "LuaEntity" }
game = {}

-- The engine's arity discipline: __index hands back a closure that already
-- carries the object, so a method takes exactly its declared arguments and
-- counts them. A function(self, ...) in a plain table is the shape that hid
-- "Arguments count error" on every method in the API for a milestone.
--
-- THREE, not the four write_file declares: fk_abi trims TRAILING absent
-- optionals rather than padding with nils, because a real nil is an argument
-- Factorio counts and then type-checks. The guest omits for_player.
local function arity(n, f)
  return function(...)
    if select("#", ...) ~= n then
      error("Arguments count error: got " .. select("#", ...) .. " want " .. n, 0)
    end
    return f(...)
  end
end
helpers = {
  write_file = arity(3, function(name, data, append)
    local h = data:gsub(".", function(c) return string.format("%%02x", c:byte()) end)
    print("LOG wrote " .. name .. " " .. #data .. ":" .. h)
  end),
}
require("control")

-- Every byte value, in order, as one Lua string. Lua strings are counted rather
-- than NUL-terminated, so this is an ordinary value here and it is the ABI's
-- job to keep it one.
local bytes = {}
for i = 0, 255 do bytes[#bytes+1] = string.char(i) end
bytes = table.concat(bytes)

-- 1. Through a TIER-2 value: a dictionary entry, which is the shape a mod's own
--    data arrives in.
handlers[42]({
  entity = ent,
  player_index = 7,
  tick = 1234,
  name = 42,
  tags = { colour = "red", count = 3, live = true, blob = bytes },
})

-- 2. Through a plain STRING FIELD on an event payload, which is the shape the
--    engine's own values arrive in -- and the shape on_udp_packet_received's
--    payload has, which is where this defect was found.
handlers[24]({
  message = bytes,
  player_index = 3,
  name = 24,
  tick = 1235,
})
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("running the mod: %v\n%s", err, out)
	}

	want := []string{
		"LOG tags: 4 colour='red' count=3 live=true blob=" + all,
		"LOG player=7 tick=1234",
		"LOG entity=iron-chest",
		"LOG chat: " + all,
		// The host's own line, over what the guest sent back.
		"LOG wrote echo.bin " + all,
	}
	got := strings.Split(strings.TrimSpace(out), "\n")
	for i := range got {
		got[i] = strings.TrimSpace(got[i])
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d lines, got %d:\n%s", len(want), len(got), out)
	}
	for i := range want {
		if got[i] == want[i] {
			continue
		}
		// A lossy read shows up as both a longer line and a different one, so
		// say which -- "these two 600-character strings differ" is not a bug
		// report. U+FFFD is efbfbd in the hex, and there will be a lot of it.
		t.Errorf("line %d is not byte-exact (got %d chars, want %d)%s:\n  got  %s\n  want %s",
			i+1, len(got[i]), len(want[i]),
			map[bool]string{true: " -- and it is full of U+FFFD, which is the lossy reader", false: ""}[strings.Count(got[i], "efbfbd") > 4],
			got[i], want[i])
	}
}
