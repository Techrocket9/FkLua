# Limits a generated guest can reach

Two Lua limits are close enough to a real guest to be worth knowing about, and both are checked when you package rather than left for your users to discover. This page says what each error means and what to do about it.

Everything else about generated Lua is bounded comfortably. A chunk's overall size is not a constraint: Factorio parses about 40 MB of Lua a second and a chunk never appears in a save, so a multi-megabyte module costs a fraction of a second at load and nothing thereafter.

## "function X is too big for Lua"

```
fklua mod: function main.onData is too big for Lua: one jump inside it crosses
990047 bytes of generated Lua and the limit is about 655355 ... one hop of the
ladder covers 327677 bytes, and the longest run inside this span with no
body-level statement boundary in it is 990038 bytes ...
```

### What it means

Lua 5.2 encodes a jump offset in an 18-bit field, so no single jump inside one function may span more than 131,071 virtual-machine instructions. Past that the Lua parser refuses the entire file with

```
control structure too long near 'trap_unreachable'
```

The line it points at is generated Lua, not anything you wrote, and there is no route from it back to your Go or Rust. It appears when a player starts the game rather than when you build. The package-time check exists to move it to where you can act on it.

The limit is on **one jump's span**, not on how big a function is. A function with no branch in it can be arbitrarily large: a 140,998-instruction function with no jump loads fine. What reaches the limit is a large function that also contains a branch crossing most of it.

### Why it happens, and why it is not about the code you wrote

Nobody writes a function containing a hundred thousand instructions. The optimizer builds one. At `-opt=2` LLVM inlines whole sections of a program into a single function, and it merges every trapping edge in that function into one block at the bottom. In the generated Lua that block is a label followed by `trap_unreachable()`, so the first bounds check near the top of the function ends up jumping over the entire body.

That makes the shape most likely to hit it a **data-stage guest**: a few hundred straight-line prototype definitions, split across section functions for readability, which the optimizer then concentrates into one. A control-stage guest reaches it less often because its work is spread across event handlers, but nothing prevents it.

### An over-long jump is relayed before anything is refused

Packaging does not refuse a long jump on sight. It breaks the jump into hops first, by inserting trampolines at function body level and pointing the original jump at the first one:

```lua
  goto LTs3            -- the straight-line path steps over the station
  ::LT3::              -- the trampoline the previous hop lands on
  if 0 ~= 0 then end   -- one statement, so the hops do not merge back into one jump
  goto LT4             -- the next hop, or the original target
  ::LTs3::
```

One hop covers half the threshold, 327,677 bytes, so a span of any size becomes a ladder of hops that each fit. Nothing moves and no function is added; only the jump is broken up. A relayed module loads, and the message above appears only for the shape the ladder cannot help.

**That shape is a block, not a distance.** A trampoline is a label, and a Lua `goto` may leave a block but never enter one, so a station can only go at the function's own body level. If a run of straight-line code longer than one hop sits inside a single `if` body or loop body, there is nowhere in it to put a station and the ladder cannot be built. The distance the message quotes as "the longest run with no body-level statement boundary in it" is that run.

So the number that bounds a large guest is not the span. The span is handled. What is left is how long a nested straight-line run gets, against the 327,677 bytes one hop covers.

### The fix

Keep the section boundaries you already have, by telling the optimizer not to inline across them.

In Go:

```go
//go:noinline
func weapons() {
	// ...
}
```

In Rust:

```rust
#[inline(never)]
fn weapons() {
	// ...
}
```

Put one on each function you think of as a section. Measured on a reproduction of the reported shape: twenty section functions of sixteen prototypes each are inlined into one whose jump crosses 1,556,741 bytes and is refused, and the same source with the pragmas packages without complaint.

Splitting a function by hand works too, but only if the optimizer keeps the split: a small function called once is exactly what inlining removes, so the pragma is what makes the split stick.

If the message quotes a block close to the whole span, the run is nested inside one construct: a long straight-line body inside a loop, or inside one branch of an `if`. Lifting that run to the function's own body level is enough on its own, because a long straight-line run at body level has a statement boundary every few bytes and can be relayed at any size.

One guest reported its emitted module coming out 27% smaller once its sections stopped being inlined. That is a real measurement and it is not a rule. A reproduction of the same shape came out 0.2% larger. Do not expect a size change in either direction.

### The threshold, and why it is in bytes

The check counts bytes of generated Lua between a jump and its target, not instructions, because counting virtual-machine instructions would mean reproducing Lua's own code generator. The conversion between the two was measured rather than estimated: every guest in the FkLua repository was compiled at three optimization levels in both languages, and each chunk was dumped and read back as Lua bytecode for its real instruction counts and jump offsets. Over 2,713 generated functions, a jump long enough to matter spans between 5.6 and 8.0 bytes of Lua per instruction.

The threshold ships at five bytes per instruction, so 655,355 bytes, which is about 11% inside Lua's own limit. The widest jump in any guest FkLua ships is 248,744 bytes, 38% of the threshold. The margin is deliberately on the side of refusing a module that would just barely have loaded: a guest inside that last 11% is one prototype away from not loading at all, and the alternative is your users meeting the error instead of you.

### Watching the distance before it refuses

Packaging prints what each module's widest jump cost, under the line that names that module. Each of the two lines below is one line as the tool prints it:

```
  jump span: widest 954969 bytes in (*github.com/Techrocket9/fkrecipes/go.Lib).PlanData (146% of the 655355-byte limit), relayed through 11 station(s) in 1 function(s); widest span after the relay 328189 bytes (50% of the limit); widest block 802 bytes against the 327677 one hop covers, so 326875 bytes of block room (100%)
  the relay carried this module, so the number that bounds it is the block room and not the span: a station is a label, a label only goes at function body level, and the relay refuses when a run with none in it is longer than one hop. Keep a long straight-line run at the function's own body level rather than nested inside one if or loop body, and put //go:noinline (Go) or #[inline(never)] (Rust) on your section functions so no single function carries a block that long.
```

Read the first number and the last. The first is how far the widest jump reached before anything was done about it, so 146% means this module would not have loaded. The last is the room the relay has left. The middle number is the one to ignore for headroom: after a relay the widest span is about one hop whatever the guest's size, so it reads as a comfortable half of the limit for a module that is well over it.

A module that needed no relay gets the block and the room too, because those are two independent questions: whether the relay had to run, and whether it could have. A module at 76% of the span limit whose block room is already negative is one that stops packaging on the day it crosses, and the line says so:

```
  jump span: widest 495047 bytes in $blocky (76% of the 655355-byte limit), no relay needed; widest block 495038 bytes against the 327677 one hop covers, so -167361 bytes of block room (-51%)
  this module is under the span limit and the relay could NOT carry it if it crossed: the widest run with nowhere at body level to put a station is already longer than one hop. Lift that long straight-line run to the function's own body level rather than leaving it nested inside one if or loop body, and put //go:noinline (Go) or #[inline(never)] (Rust) on your section functions so no single function carries a block that long.
```

`fklua mod --report FILE` carries the same facts per module, under `jumps.control` and `jumps.data`:

```json
"jumps": {
  "data": {
    "limit_bytes": 655355,
    "hop_bytes": 327677,
    "widest_span_bytes": 954969,
    "widest_span_function": "(*github.com/Techrocket9/fkrecipes/go.Lib).PlanData",
    "stations": 11,
    "widest_span_after_relay_bytes": 328189,
    "widest_block_bytes": 802,
    "block_room_bytes": 326875,
    "relayed": true,
    "relayed_functions": [
      {
        "function": "(*github.com/Techrocket9/fkrecipes/go.Lib).PlanData",
        "widest_span_bytes": 954969,
        "stations": 11,
        "widest_span_after_relay_bytes": 328189,
        "widest_block_bytes": 802,
        "block_room_bytes": 326875
      }
    ]
  }
}
```

`widest_span_bytes` is measured before the relay and `widest_span_after_relay_bytes` after it. `widest_block_bytes` is the longest run with no body-level statement boundary in it, measured in the function `widest_span_function` names, and `block_room_bytes` is `hop_bytes` minus it. Block room is the figure to watch as a guest grows: it goes negative exactly when the relay can no longer help, which is when the message above appears. The span it is measured in is that function's widest one while the relay can still carry the function, and the span the relay could not carry once one exists. Those two need not be the same jump: a function's jumps are relayed one label at a time, so a wide one can be carried while a narrower one in the same function is not, and it is the narrower one that refuses the module and that the error names. `limit_bytes` and `hop_bytes` are properties of the compiler rather than of your module, so they are present even for a module the packaging does not have; every measured number is 0 there and `relayed` is false. `relayed_functions` is a list and is `[]` when nothing was relayed. A refusal writes the report too, and its entry is the refused function's own row rather than the module's widest, so every number under `widest_span_function` there describes the one function the build stopped on.

`fklua compile` prints the same lines under its own `wrote` line.

## "the generated chunk declares N locals and Lua's limit is 200"

Lua caps a function at 200 local variables, and a chunk is a function. The FkLua runtime prelude spends most of that budget, and what is left goes on your module's **globals**: one local per global, two for a 64-bit one.

Real guests emit zero or one global, so this is rare. Measured today, 26 mutable 32-bit globals fit and 27 do not. If you reach it, reduce the module's globals. In Go and Rust that usually means a package-level or `static` mutable value that could live behind a pointer into linear memory instead.

## Related

- [Writing a mod's settings and data stages](data-stage.md), the guest shape most likely to reach the jump limit.
- [What the engine checks](data-stage.md#what-the-engine-checks). Every limit on this page is Lua's. Factorio enforces its own on the prototypes a data stage builds, including a 200-character cap on a prototype name and a 20-parameter cap on a localised string, and those are measured there.
- [Guest memory and the collector](memory.md).
- [Verifying a mod headlessly](verifying.md), which is how to find out whether what you packaged loads.
