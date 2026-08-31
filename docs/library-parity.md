# Keeping a two-language library honest

A library that ships for both Go and Rust is two hand-written implementations of one contract, and nothing generated watches them. FkLua's own generated bindings are held in step by a census that regenerating them refreshes; a hand-written pair has no census row, so the only thing standing between the two implementations and a quiet divergence is a test that runs both and compares. FkLua carries several such pairs, one of them drifted for months before that test existed, and this page is the recipe that has kept them level since.

If your library is one language, you do not need any of this. If it is two, the recipe below is cheaper than the alternative, which is a mod author filing the difference as a bug in whichever language they happened to pick.

## The shape: one script, two guests, byte-identical transcripts

1. Write an example guest per language that exercises every public function of the library, in the same order, with the same inputs. The two examples should differ in as little as possible beyond the language; a difference between them is only meaningful if the code around it is the same.
2. Build both and package each with `fklua mod`, exactly as a consumer would.
3. Run each packaged mod's generated Lua under one Lua 5.2 interpreter against a stand-in for whatever environment the library touches, with every observable effect printed: each host call the library makes, each value it writes, each line it logs, in order.
4. Compare the two transcripts byte for byte. Any difference is a finding; the line number says where.

Two details carry most of the value:

**Serialise canonically.** Print tables with sorted keys and numbers at full precision (`%.17g`), never with `tostring` on a table or a default float format. The transcript has to be a function of the values, not of an iteration order or a formatting default, or the comparison reports differences that are not there and misses ones that are.

**Make the stand-in as strict as the real thing.** A stand-in that accepts anything tests one branch of the library and hides the other. If the real environment validates, the stand-in validates; a guest that emits something malformed should fail in the harness, not in the game.

## What this finds

The comparison catches the class of difference no per-language test can see, because each language's own tests encode that language's assumptions. Real examples from the pairs FkLua maintains: constant folding that rounds differently between the two languages, so one guest emitted `0.40400000000000003` and the other `0.40399999999999997`; a sort that was stable in one implementation and not the other, visible only on equal keys; and a numeric edge case where one language's overflow behaviour differed and a comment claimed otherwise.

## The no-toolchain pin: a committed golden

The transcript comparison needs both toolchains installed. The half that does not is a committed golden file both implementations read: a text file of inputs and expected encodings that each side re-produces byte for byte, with one side's test suite doubling as the file's generator behind an update flag. This is the cheapest cross-language pin there is, and it keeps working in an environment that can build neither guest. Use it for anything wire-shaped: an encoding, a digest, a rendering.

## Which interpreter

Both arms must run under the same interpreter; that is what makes a transcript difference attributable to the guests. A stock Lua 5.2 works for parity. If your library's behaviour leans on the sandbox's numeric shape (doubles-only arithmetic, `string.pack` truncation), use an interpreter patched to match Factorio, which the FkLua repository builds for its own tests; the closer the interpreter, the more a transcript means. And know one property of stock Lua 5.2 before you reason about a harness fixture: `pairs()` order over string keys varies between runs, because the string hash is seeded from the clock. Sort in the harness, never rely on a fixture's iteration order, and be suspicious of a test that passes on some runs.

## Running a packaged mod outside the game

The mechanics of loading a packaged mod's generated files under an interpreter, with `require` paths and a stand-in environment, are the same ones used for debugging and are described in [debugging.md](debugging.md). The parity harness is that setup, run twice, plus a diff.
