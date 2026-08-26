# Limits a generated guest can reach

Two Lua limits are close enough to a real guest to be worth knowing about, and both are checked when you package rather than left for your users to discover. This page says what each error means and what to do about it.

Everything else about generated Lua is bounded comfortably. A chunk's overall size is not a constraint: Factorio parses about 40 MB of Lua a second and a chunk never appears in a save, so a multi-megabyte module costs a fraction of a second at load and nothing thereafter.

## "function X is too big for Lua"

```
fklua mod: function main.onData is too big for Lua: one jump inside it crosses
778357 bytes of generated Lua and the limit is about 655355 ...
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

One guest reported its emitted module coming out 27% smaller once its sections stopped being inlined. That is a real measurement and it is not a rule. A reproduction of the same shape came out 0.2% larger. Do not expect a size change in either direction.

### The threshold, and why it is in bytes

The check counts bytes of generated Lua between a jump and its target, not instructions, because counting virtual-machine instructions would mean reproducing Lua's own code generator. The conversion between the two was measured rather than estimated: every guest in the FkLua repository was compiled at three optimization levels in both languages, and each chunk was dumped and read back as Lua bytecode for its real instruction counts and jump offsets. Over 2,713 generated functions, a jump long enough to matter spans between 5.6 and 8.0 bytes of Lua per instruction.

The threshold ships at five bytes per instruction, so 655,355 bytes, which is about 11% inside Lua's own limit. The widest jump in any guest FkLua ships is 248,744 bytes, 38% of the threshold. The margin is deliberately on the side of refusing a module that would just barely have loaded: a guest inside that last 11% is one prototype away from not loading at all, and the alternative is your users meeting the error instead of you.

## "the generated chunk declares N locals and Lua's limit is 200"

Lua caps a function at 200 local variables, and a chunk is a function. The FkLua runtime prelude spends most of that budget, and what is left goes on your module's **globals**: one local per global, two for a 64-bit one.

Real guests emit zero or one global, so this is rare. Measured today, 26 mutable 32-bit globals fit and 27 do not. If you reach it, reduce the module's globals. In Go and Rust that usually means a package-level or `static` mutable value that could live behind a pointer into linear memory instead.

## Related

- [Writing a mod's settings and data stages](data-stage.md), the guest shape most likely to reach the jump limit.
- [Guest memory and the collector](memory.md).
- [Verifying a mod headlessly](verifying.md), which is how to find out whether what you packaged loads.
