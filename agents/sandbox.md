# The Factorio Lua 5.2 sandbox

Everything here was verified against the real interpreter or the running game, not inferred. `third_party/lua-5.2.1/sandbox_check.lua` re-checks the structural claims on every push; the measured numbers come from the day-0 in-game probe.

## Hard walls

These silently produce broken code if you forget them. Reference:
<https://lua-api.factorio.com/latest/auxiliary/libraries.html>

| Wall | Consequence for codegen |
|---|---|
| **Lua 5.2.1**, no JIT | Doubles only; exact integers to 2⁵³ |
| **No `coroutine`** | Cannot yield mid-function. A call must finish inside one tick. |
| No `os`, no `io` | No wall clock at all. `helpers.create_profiler()` is the only timer, and it refuses to hand Lua a raw number. |
| **`load()` rejects binary chunks** | Emit source, never bytecode. Killed the Phobos compiler. |
| No bitwise operators | `bit32.*` function calls only — but see "prefer arithmetic" below |
| **200 locals per function** | 255 registers including temporaries; target ≤180 declared. **Verified**: 199 ok, 200 ok, 201 rejected. A chunk is a function, so it is capped too — see the chunk-local budget in [`agents/codegen.md`](codegen.md). |
| **A jump spans at most 131,071 VM instructions** | `MAXARG_sBx`. Past it the parser refuses the whole chunk with `control structure too long`, naming nothing at all. **Verified**: 131,071 loads, 131,072 is rejected. See "A jump is 18 bits" below. |
| `LUAI_MAXCCALLS` = 200 parser nesting | Why control flow is flat `goto`, not nested `while`/`break` |
| **Functions error on save in `storage`** | Generated functions are rebuilt by `require`, never persisted |
| Unregistered metatables are stripped on save | Never put a metatable on `MEM` without `script.register_metatable` |
| **There is no `on_save` event** | Anything persisted must be serializable in `storage` at all times |
| `storage` absent at `control.lua` top level | Declare `MEM` at chunk level, assign it in `on_init`/`on_load` |
| Writing `storage` in `on_load` errors | `on_load` is read-only and must be deterministic |
| No instruction budget | An infinite guest loop hangs everyone's game; `--fuel=N` stops one, and **defaults off** because it is not cheap — see [`agents/codegen.md`](codegen.md) |
| Deterministic lockstep | Anything nondeterministic in guest code desyncs every client |

**Verified present**: `bit32` (complete, and unsigned — `bit32.bnot(0) == 4294967295`), `math.frexp`/`math.ldexp`, `goto`/labels, hex float literals, `pcall`/`xpcall`/`select`, `table.concat`/`table.unpack`, `debug.getinfo`/`debug.traceback`.

**Verified absent**: `coroutine`, `io`, `os`, `loadfile`, `dofile`, `math.type`, integer division `//`, bitwise operators, all of `debug` beyond the two above.

---

## The 2²⁰ table wall — where `bin/lua52f` stops being Factorio-shaped

Everything above is a fact about the LIBRARY, and the oracle reproduces all of it. **Table internals are a different matter and the oracle does not model them.** Measured in Factorio 2.0.77 with a bare Lua mod containing no guest:

| | Factorio | `bin/lua52f` |
|---|--:|--:|
| 200k stores into keys 1..200k of a 1,000,000-key table | 24 ms | — |
| the same 200k stores, table grown to 1,100,000 keys | **482 ms** | — |
| grow 888,832 → 1,111,040 words (crosses 2²⁰) | **2,716 ms** | 3.0 ms |
| grow 1,111,040 → 1,333,248 words (does not) | 101 ms | 1.3 ms |

**Past 2²⁰ = 1,048,576 keys a Lua table in Factorio stops behaving like an array for ALL of its keys** — the low indices too — and every access costs ~20× more. `lua52f` is stock 5.2.1, whose array part grows to 2³⁰, so it shows a 2.3× slope where the game has a 27× cliff.

Two consequences, the second of which has already cost this project a wrong explanation:

- **A guest's linear memory should stay under 4 MiB** — one word per slot, so 4 MiB is exactly the wall. See [`agents/gc.md`](gc.md), "The 4 MiB wall", and the heap budget in [`agents/guests.md`](guests.md). **The wall is bracketed to 4,096 words**: 1,048,576 words is still an array at 108 ns per access and 1,052,672 is not, at 3,820 — see [`agents/sharding.md`](sharding.md), which is also where the representation that removes it is designed.
- **The oracle is ~2.5× fast on ANY table access, not only a large one.** The identical emitted access loop over the identical 524,288-word table is 31.0 ns under `bin/lua52f` and 56–73 ns in game, with the loop machinery itself at 1.04–1.10× — so the table read alone is **4–6×**. That constant is what carries a host-side timing into the game, and until it was measured several published numbers assumed it was 1. [`agents/sharding.md`](sharding.md) §2.
- **No host-side measurement of a large table transfers, in either direction.** `agents/gc.md` derived a "~19 rehashes" account of the 2.8-second grow from the vendored `ltable.c`; the vendored `ltable.c` is not what Factorio ships, and every fix that account implies was measured in game and changes nothing. Reason about a big table from the game — not from the oracle, and not from `third_party/lua-5.2.1`.

---

## A jump is 18 bits — the wall a big generated function hits

**Every jump in a Lua function is one VM instruction whose signed offset lives in the `sBx` field, and the field is 18 bits biased.** `SIZE_Bx = SIZE_B + SIZE_C = 9 + 9` (`lopcodes.h`), so `MAXARG_sBx = 2¹⁷ - 1 = 131071`, and `lcode.c`'s `fixjump` refuses to patch anything wider:

```c
static void fixjump (FuncState *fs, int pc, int dest) {
  int offset = dest-(pc+1);
  if (abs(offset) > MAXARG_sBx)
    luaX_syntaxerror(fs->ls, "control structure too long");
```

**Verified against `bin/lua52f` at the instruction, in both directions**: a forward `goto` over exactly 131,071 single-instruction statements loads, and 131,072 is refused. `TestTheJumpLimitIsWhereLuaPutsIt` re-checks it on every run, because a constant read out of a header is a constant nobody checked.

Four things about it that are easy to get wrong, and each has already cost something:

- **The unit is VM INSTRUCTIONS. Not lines, not bytes, not statements.** A single emitted line can be one instruction or fifty — a loop guard's seed measures at 49 in this repo's own output.
- **The limit is on ONE JUMP'S SPAN, not on a function's size.** A function with no jump in it is unbounded, and that is measured rather than argued: the data-guest reproduction built without its bounds checks emits a **140,998-instruction function whose widest jump is zero**, and it loads. Anything that checked function size would refuse it for nothing.
- **`abs(offset)`, so a long backward branch counts too.** A loop whose body is enormous fails at its back edge.
- **The diagnostic names nothing.** The message carries the token the parser happened to be holding when the pending gotos were patched, which is whatever follows the label — for a generated guest that is usually `control structure too long near 'trap_unreachable'`. No file, no function, no mod. That is the whole reason [`agents/codegen.md`](codegen.md) has a package-time check for it.

**The failure needs a big function AND a long jump, and an optimizer supplies both.** LLVM at `-opt=2` inlines a program's sections into one function and merges every trapping edge into ONE block at the bottom; the emitter renders that block as a label followed by `trap_unreachable()`, so the first bounds check in the function jumps over the entire body. Reported by a downstream data guest that compiled to a 28,139-line Lua function, and reproduced here from real Go through the real toolchain.

Other Lua limits nearby, none of which binds first: a function's instruction count is capped at `MAX_INT`, its constants at `MAXARG_Bx` = 262,143 before `LOADKX` takes over, and its registers at `MAXARG_A` = 255 (which the 200-local rule already keeps clear of).

---

## `collectgarbage` is present, and it is a ~10% lever — not the one people reach for

**Verified in game, 2.0.77** (`probe.json`: `g_collectgarbage`, `cg_count`, `cg_step`, `cg_collect`, `cg_isrunning`, `cg_setpause`, `cg_setstepmul` — all true). The day-0 probe never asked, and the gap cost a downstream mod two milestones of mis-attribution, so it is asserted in `sandbox_check.lua` now.

It does **not** solve the thing anyone reaches for it to solve, and the size of what it *does* solve changed under it — which is why this section used to say "not a lever" flatly and now does not.

**The original measurement, and it stands for the world it was taken in.** A guest's linear memory was ONE enormous Lua array table, and Lua 5.2 traverses a table in a single `propagatemark` — one gray object, one indivisible unit of work, so the whole table is walked inside one tick. Pacing parameters choose how many *objects* a step covers; against one object they do nothing. Measured: a 64 MiB heap's worst tick is 12.69 ms at the defaults and 12.51–12.74 ms across every `setpause`/`setstepmul` combination tried, inside the same configuration's own run-to-run noise.

**SHARDING RETIRED THAT PREMISE.** Linear memory is a vector of 2¹⁹-word shards now, so there are many gray objects rather than one, and there IS something to pace. Re-measured: one `collectgarbage("step", 2)` per tick is **0.93× worst, 0.89× p99, 0.87× mean** on a 26-shard write arm, and 7.711 ms against 11.428 on the paced-vs-unpaced pair. So the honest statement is **~10%, and still not a fix** — 10% off an 11 ms tick is 10 ms, and the thing that actually removes the pause is a smaller heap or `--gc=collected`.

Where the two numbers disagree, the SHARDED one is current and the flat "not a lever" claim is superseded; `agents/guests.md`'s "mitigations that were measured and lost" carries the pre-sharding measurement and says so in place. See [`agents/guests.md`](guests.md), "the guest heap budget", for the curve and for what does work.

Two cautions if a guest or a runtime ever does call it. The Lua state is **shared by every mod**, so `setpause`/`setstepmul` retune the collector for all of them — the probe puts the defaults back. And `collectgarbage("count")` is a host-memory number that differs between machines: reading it is fine, letting it reach anything the simulation depends on is a desync.

---

### Lua constraints the emitter is built around, verified against the interpreter

| Constraint | Consequence |
|---|---|
| A goto into a **sibling block** is rejected | Everything is emitted FLAT, at function-body level. This is why control flow cannot use nested `while`/`break`. |
| A bare mid-block `return` is a **syntax error** | Every early return is wrapped in `do ... end` |
| A **duplicate label** is rejected | A loop defines its label at its TOP only; its `end` defines nothing |
| `goto` may be followed by more statements | Unlike `break`, no "last statement" restriction |
| A label may sit immediately before `end` | Block-end labels need no filler statement |

---

## `string.pack` — settled by the probe, and now in the oracle

**`bin/lua52f` carries the backport as of M6** (`patches/02-string-pack.patch` plus `third_party/lua-5.2.1/strpack.c`, an ordinary file so it can be diffed against upstream 5.4.6). Before that it did not, and `sandbox_check.lua` tested it behind `if string.pack then` — so the branch never ran, the oracle silently lacked a feature the game has, and CI's own comment claimed it was checked. The check now asserts, and was verified to FAIL against a build without the patch.

The semantics below are what the backport reproduces, value for value.


Factorio's 5.4.6 backport runs on a doubles-only build, and it behaves like **5.2's truncating cast, not 5.4's strict conversion**:

| Expression | Result | Consequence |
|---|---|---|
| `string.pack("<I4", 3.5)` | packs **3** | **Truncates. Does not raise** "number has no integer representation" the way real 5.4 does. |
| `string.pack("<I4", 4000000000)` | round-trips exactly | u32 is safe |
| `string.pack("<I8", 2^53+1)` | comes back **2^53** | `<I8>` cannot carry an arbitrary i64 — the double cannot represent the value in the first place |
| `string.pack("<i8", -1)` | round-trips | signed 8-byte works for small magnitudes |
| `string.unpack("<I4I4", string.pack("<d", 1.0))` | `lo=0 hi=1072693248` | little-endian f64 punning is exact (0x3FF0000000000000) |
| `bit32.band(3.7, 0xFFFFFFFF)` | **3** | `bit32` truncates the same way |

So: **use `<I4I4>` pairs for i64, never `<I8>`.** That is the representation we use anyway, and it is lossless because each half is below 2³².

**`lua52f` must copy the truncating behaviour, not 5.4's.** Porting 5.4's `lstrlib.c` verbatim would raise where Factorio silently truncates, and the oracle would then reject programs the game accepts.
