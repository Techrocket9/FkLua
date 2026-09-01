# Debugging a guest

There is no debugger for a running FkLua mod: your program is not in the Lua state, so Factorio's own instrument mode has nothing to attach to. What there is instead is four ways to make the guest say what it is doing, a loop that turns a question into an answer in about a minute, and a map that says which of your functions a line of generated Lua belongs to.

## The four instruments

**`fk.Log(s)` writes a line to Factorio's log.** That is the workhorse. The log is not checksummed, so it is also the only sanctioned place for anything peer-local: a duration, a memory figure, anything that can legitimately differ between two clients. Headless, it goes to the terminal and to `factorio-current.log`.

**`fk.Print(s)` writes to the in-game console.** Before the game exists (during mod load, and in a load hook) there is no console and the message goes to the log instead, so it is safe to call anywhere.

**`fk.LastError()` is what the engine said.** A host call answers with a status, and a status is a number: `ERR_CALL_FAILED` tells you the API raised and not what it raised about. `LastError` carries the engine's own sentence, and it is cleared at the start of every host call, so it means "the call that just returned" rather than "whatever failed last".

```go
if _, err := surf.CreateEntity(spec); err != nil {
    fk.Log("create_entity: " + err.Error() + ": " + fk.LastError())
}
```

**`Value.Dump(dst)` renders a tier-2 value into a buffer you own.** The accessors (`Get`, `AsStr`, `NumOr`) answer a question you already knew to ask; this is for when you do not know what you were handed.

```go
fklog.Start("payload ")
fklog.Advance(v.Dump(fklog.Tail()))
fklog.End()
```

It writes into a destination rather than returning a string, because a string would allocate and the allocation would be permanent under the leaking collector. `fklog.Tail()` lends the rest of the current line and `fklog.Advance()` records what was written; any other `[]byte` works the same way. A value bigger than the destination is truncated and the return is what fitted.

The rendering is Lua-ish and is for a person reading a log. Nothing parses it back:

```
{name="belt", count=42, on=true, list=[1, "two", false], inner={deep=7}, [7]="seven"}
```

## Build the line, do not concatenate it

`fk.Log("cluster " + itoa(n) + " parts " + itoa(m))` allocates four strings you throw away, and a guest's heap is in every save and every multiplayer join. One mod measured its entire guest heap as log lines. `fklog` builds a line in one fixed buffer and hands the host a string that borrows it, so a line costs nothing:

```go
import "github.com/Techrocket9/fklua/guest/go/fklog"

fklog.Start("[mymod] compiled cluster ")
fklog.U(uint64(root))
fklog.S(" parts=")
fklog.U(uint64(n))
fklog.End()
```

In Rust the same functions are `fklog::start`, `fklog::u`, `fklog::s`, `fklog::end`, and the crate is `fklog = { path = "..." }` beside `fk` and `fkapi`.

Two things to know about it. A line longer than the buffer is **truncated rather than grown**, which is what keeps the appenders one memcpy each. And **you may not make a host call between `Start` and `End`**: there is one buffer, and an event Factorio raises synchronously from inside a call you made would build its own line over the top of yours.

## Reading a generated-Lua line back to your own code

A traceback or an error from a running mod points at `fk_module.lua` and a line number, which is true and tells you nothing. `fklua mod` writes `fk_module.map.json` beside the module to close that gap: it lists every function in the module with the range of Lua lines it occupies, its name, and, when the guest carries debug information, the source file and line it was defined at. Look up the line, get the function.

```json
{ "lua": [9929, 10997], "wasm": 44, "name": "main.onTick", "src": "main.go", "line": 77 }
```

So `fk_module.lua:10004` is somewhere inside `main.onTick`, written at `main.go:77`. The format is described in [generated-files.md](generated-files.md); the short version is that `functions` is sorted by first line and the ranges do not overlap, so a lookup is a binary search. It is meant to be read by tooling as much as by a person: a debug-adapter proxy annotating stack frames, or a filter over log output that rewrites `fk_module.lua:N` into a function name.

What it gives you is attribution and nothing more. There are no breakpoints, no stepping and no variables, and the map does not change where execution happens: the Lua location stays the real location, and the map only says whose code that is. It is function granularity, not statement granularity, so it will not tell you which line of `onTick` you were on.

A Go guest built with the flags the scaffold documents already carries the debug information the source positions come from. A Rust guest carries it because the scaffolded release profile sets `debug = "line-tables-only"`, which covers most functions and costs no generated Lua at all. Setting `debug = true` instead covers every function, at a much larger wasm (measured on the scaffolded guest: 82,959 bytes at line tables, 531,423 with full debug information, against 24,599 with neither). A guest built with no debug information still gets a map, with names and line ranges and no source positions.

`fklua mod --no-map` leaves the file out.

## The loop

Most questions are answered without launching the game at all. `bin/lua52f` is a Lua 5.2.1 built to match Factorio's sandbox, and a packaged mod is ordinary Lua, so it can be driven directly with stand-ins for the game globals:

```sh
make lua52f
./bin/fklua mod guest.wasm -o /tmp/m
cat > /tmp/drive.lua <<'LUA'
package.path = "/tmp/m/my-mod_0.1.0/?.lua"
function log(s) print("LOG " .. s) end
defines = { events = {} }
storage = {}
local handlers = {}
script = {
  mod_name = "my-mod",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
game = { get_surface = function(_) return { valid = true, object_name = "LuaSurface" } end }
helpers = {}
require("control")
handlers.on_init()
LUA
./bin/lua52f /tmp/drive.lua
```

Stub only what your guest calls. A stubbed object needs `valid = true` and, for anything the bindings ask about, `object_name`; a method is a plain function in the table, called with its arguments and no receiver.

**Never test against a system `lua`.** Anything but 5.2 has an integer subtype, so `%`, overflow and `string.pack` all differ from Factorio's doubles-only 5.2.1, and it will silently pass code that breaks in game.

What the oracle cannot answer: whether the mod loads (the mod format, `require` resolution and Factorio's own parse), and anything about the cost of a large Lua table, which the oracle reads several times faster than the game does. For those, run it: [verifying.md](verifying.md) is the headless create-and-benchmark recipe, and it doubles as the determinism check.

## When the mod does not load at all

Read `factorio-current.log` in your Factorio user directory. Three failures account for most of them:

- **A pin mismatch.** A guest built against one `runtime-api.json` packaged with tables from another is refused at package time, naming both versions. Regenerate the bindings, or set the pin in `fklua.toml`.
- **A stale wasm.** The guest was built before the last `gen-bindings` at the same pin. The packager names it on stderr with both digests and the repair; the bindings and the wasm are one pair.
- **An engine mismatch.** `info.json`'s `factorio_version` is a claim about the ENGINE, and a 2.1 engine refuses a mod declaring 2.0 outright at game start. It defaults from the API pin, and `--factorio-version` overrides it.

## What is out of scope, and why

Factorio's instrument mode injects Lua into a mod's own state to debug it. An FkLua guest's program is not in its Lua state, so there is nothing there for it to reach: the debugger for a guest is a Go or Rust debugger, run against the same source with the host calls stubbed.

The debug map does not change that. It names the function a Lua line belongs to, which is enough to read a traceback and to label a stack frame, and it is not a source-level debugger: stepping, breakpoints and variable inspection would all need the guest's state to be visible from Lua, and it is not.
