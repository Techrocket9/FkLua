package factorio

import (
	"strings"
	"testing"
)

// A member returning a plain string must bracket its call with the allocation
// mark, because the HOST allocates for it.
//
// `write_field`'s K_STRING branch in runtime/lua/fk_abi.lua calls `alloc_(n)`
// to find somewhere in guest memory to put the bytes, exactly as an array or a
// dynamic value does. What makes the string different is only that its layout
// is fixed, so it goes through `write_field` rather than `write_value` -- and
// the generator's `allocs` predicate was written from that list of kinds rather
// than from the question it is actually asking, which is "does anything here
// call fk_alloc".
//
// So `entity.name` pinned a buffer and nothing ever released it. The guest
// binding copies the bytes into its own string type immediately and never looks
// at the pointer again, so nothing was corrupted and no test failed -- the pin
// list simply grew by one entry per call, forever. A mod reading a name in
// on_tick appends sixty entries a second to a slice nothing shortens, in a
// lockstep game where every client does the same.
//
// The bracket is O(1) (it truncates the pin list), so covering strings costs a
// pair of integer operations on a path that already crosses the host boundary.
func TestAStringReturnReleasesWhatTheHostAllocatedForIt(t *testing.T) {
	a, err := LoadAPI(apiPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("go", func(t *testing.T) {
		g, err := cachedGo(t, a)
		if err != nil {
			t.Fatal(err)
		}
		bodies := bodiesReturning(g.Source, ") (string, error) {")
		if len(bodies) == 0 {
			t.Fatal("no member returns a plain string; the API or the generator changed shape")
		}
		var bad, forwarded int
		for _, b := range bodies {
			// An INHERITED member is a one-line forwarder to the parent's
			// binding, which owns the bracket. Requiring one here would mean a
			// second mark around a call that already has one -- correct, and
			// pure cost on a path that crosses the host boundary anyway.
			if strings.Contains(b, "{o.Object}.") {
				forwarded++
				continue
			}
			if !strings.Contains(b, "allocMark()") {
				bad++
			}
		}
		if forwarded == 0 {
			t.Error("no string-returning member is inherited; the forwarding pass " +
				"stopped running and this test's exemption is now hiding real cases")
		}
		if bad > 0 {
			t.Errorf("%d of %d string-returning Go members never release the buffer the "+
				"host allocated for them; the arena is restored per call, so the "+
				"buffer is handed away while the binding still holds the pointer",
				bad, len(bodies)-forwarded)
		}
	})

	t.Run("rust", func(t *testing.T) {
		rs, err := cachedRust(t, a)
		if err != nil {
			t.Fatal(err)
		}
		// A Rust method's body ends at its own INDENTED brace, not at a
		// column-zero one: the methods live inside `impl X { … }`. Ending at
		// "\n}\n" ran every body on to the end of the whole impl block, so a
		// method sharing a block with any bracketed member passed for free --
		// this arm was only ever reporting whole BLOCKS that contained no
		// AllocMark at all, which is why the 2026-08-03 forwarder pass (whose
		// blocks contain no host call by construction) turned it red.
		bodies := bodiesReturningUntil(rs.Source, "-> Result<LuaStr, Status> {", "\n    }\n")
		if len(bodies) == 0 {
			t.Fatal("no member returns a plain LuaStr; the API or the generator changed shape")
		}
		var bad, forwarded int
		for _, b := range bodies {
			// An INHERITED member is a one-line forwarder to the parent's
			// binding, which owns the bracket -- the same exemption the Go arm
			// makes and for the same reason: a second mark around a call that
			// already has one is correct and pure cost. A real body names the
			// handle as `self.0.0` inside fk_call; a forwarder wraps it as
			// `Parent(self.0).member(...)`.
			if strings.Contains(b, "(self.0).") {
				forwarded++
				continue
			}
			if !strings.Contains(b, "AllocMark::new()") {
				bad++
			}
		}
		if forwarded == 0 {
			t.Error("no string-returning member is inherited; the forwarding pass " +
				"stopped running and this test's exemption is now hiding real cases")
		}
		if bad > 0 {
			t.Errorf("%d of %d String-returning Rust members never release the buffer the "+
				"host allocated for them", bad, len(bodies)-forwarded)
		}
	})
}

// bodiesReturning collects the text between each occurrence of sig and the
// blank line that ends that function, which is enough to see whether the
// bracket was emitted at the top of it.
func bodiesReturning(src, sig string) []string {
	return bodiesReturningUntil(src, sig, "\n}\n")
}

// bodiesReturningUntil is bodiesReturning with the terminator named, because a
// Go top-level func and a Rust inherent method do not end at the same column.
func bodiesReturningUntil(src, sig, term string) []string {
	var out []string
	for i := 0; ; {
		j := strings.Index(src[i:], sig)
		if j < 0 {
			return out
		}
		start := i + j
		end := strings.Index(src[start:], term)
		if end < 0 {
			return out
		}
		out = append(out, src[start:start+end])
		i = start + end
	}
}

// THE SAME QUESTION IN THE OTHER DIRECTION: a member whose ARGUMENT is a struct
// must bracket its call, because encoding that struct can call the allocator.
//
// A generated `encode_at` writes each field in turn, and a field that is an
// array or a dictionary needs somewhere to put its elements -- `galloc(...)` in
// Rust, `arenaAlloc` behind `block` in Go. So `LuaTrain::set_schedule` allocates
// for `TrainSchedule.records`, `LuaSurface::set_map_gen_settings` for three
// dictionaries and a vector, and `LuaSurface::request_path` for a container that
// is a field OF a field.
//
// Rust did not bracket any of them until 2026-08-07, and the reason is worth
// keeping because it is how a two-backend generator hides a hole: the `allocs`
// predicate is character-for-character identical in gogen.go and rustgen.go, and
// neither loop tested KindStruct. Go emitted the bracket anyway, from a
// SEPARATE clause -- `args.Size > 0`, which is about its argument blocks being
// arena memory rather than about the struct. Two backends that agree through a
// third condition look exactly like two backends that agree, and the committed
// Rust bindings had 301 members with an unbracketed struct encode while the Go
// ones had none.
//
// So this asserts the two SETS are the same size rather than asserting a
// property of each independently, which is TestBothBackendsBindTheSameMembers'
// reasoning: a hole in one backend is invisible in a per-backend test that the
// other backend also passes.
//
// It is LATENT rather than live today and saying so is part of the record.
// Rust's AllocMark is an empty struct with an empty Drop (see fkapi's own doc
// comment on it), because `fk_alloc` there is the global allocator rather than a
// marshalling arena -- so nothing is reclaimed at the bracket, with it or
// without it. What this pins is the SHAPE, against the day guest/rust/fk grows a
// real arena; guest/rust/fk/src/lib.rs says that in as many words at fk_alloc.
// The Go arm is live: allocRelease really rewinds, and TestAHostCallKeepsNoHeap
// measures it through a running guest.
func TestAStructArgumentIsBracketedInBothBackends(t *testing.T) {
	a, err := LoadAPI(apiPath)
	if err != nil {
		t.Fatal(err)
	}

	g, err := cachedGo(t, a)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := cachedRust(t, a)
	if err != nil {
		t.Fatal(err)
	}

	// The encode of a struct INTO THE ARGUMENT BLOCK, which is the only place a
	// guest-side encode happens. A return block is decoded, never encoded.
	goTotal, goBad := countBracketed(bodiesReturning(g.Source, "\nfunc ("),
		".encodeAt(&a[", "allocMark()")
	rsTotal, rsBad := countBracketed(
		bodiesReturningUntil(rs.Source, "\n    pub fn ", "\n    }\n"),
		".encode_at(&mut a[", "AllocMark::new()")

	if goTotal == 0 || rsTotal == 0 {
		t.Fatalf("no member encodes a struct into its argument block (go %d, "+
			"rust %d); the API or the generators changed shape and this test is "+
			"asserting nothing", goTotal, rsTotal)
	}
	if goTotal != rsTotal {
		t.Errorf("%d Go members encode a struct argument and %d Rust ones do; "+
			"the two backends bind the same members, so this is a generator "+
			"divergence rather than an API fact", goTotal, rsTotal)
	}
	if goBad > 0 {
		t.Errorf("%d of %d Go members encode a struct argument outside any "+
			"allocMark bracket; encodeAt allocates for a container field and the "+
			"arena is never rewound, so that is guest heap kept per call -- into "+
			"the save, and into every multiplayer join", goBad, goTotal)
	}
	if rsBad > 0 {
		t.Errorf("%d of %d Rust members encode a struct argument outside any "+
			"AllocMark bracket. This is LATENT -- AllocMark is a no-op while "+
			"fk_alloc is the global allocator -- but it is the shape that becomes "+
			"a per-call leak the day guest/rust/fk grows a real marshalling "+
			"arena, which is the change its own fk_alloc doc comment names",
			rsBad, rsTotal)
	}
}

// countBracketed returns how many bodies contain needle, and how many of those
// do not also contain bracket.
func countBracketed(bodies []string, needle, bracket string) (total, bad int) {
	for _, b := range bodies {
		if !strings.Contains(b, needle) {
			continue
		}
		total++
		if !strings.Contains(b, bracket) {
			bad++
		}
	}
	return total, bad
}
