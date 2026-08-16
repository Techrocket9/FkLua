// Command kernels is the cross-language benchmark guest: the same six kernels
// written in Go, in Rust and in hand-written Lua, so the three can be compared
// on identical work.
//
// This is a DIFFERENT question from the two benchmarks that already exist.
// `bench/kernels/` is hand-written Lua modelling what the emitter produces, and
// pins a ceiling. `bench/wasm/` is hand-written .wat compiled by the real
// compiler, and measures the passes. Neither one runs a real toolchain, so
// neither can say what a mod author actually gets: TinyGo and rustc emit their
// own runtime, their own allocator and their own idea of how to lay out a
// struct, and all of that lands in the Lua too.
//
// Every kernel returns a checksum, and the harness refuses to report a timing
// unless all three languages return the SAME one. A variant that computes a
// different answer is not a faster variant.
//
//	tinygo build -target=wasm-unknown -scheduler=none -gc=leaking -o k.wasm .
package main

func main() {}

// ---------------------------------------------------------------------------
// PURE kernels: tight loops over numbers and memory, no allocation.
//
// These are what a microbenchmark usually means, and they flatter a compiled
// guest: there is no runtime involved, so the comparison is emitter against
// Lua interpreter and nothing else.
// ---------------------------------------------------------------------------

var words []uint32

//go:wasmexport pure_setup
func pureSetup(n int32) {
	words = make([]uint32, n)
	for i := range words {
		words[i] = uint32(i) * 2654435761
	}
}

// sum: a u32 array reduction. The archetypal memory-bound loop.
//
//go:wasmexport pure_sum
func pureSum(passes int32) uint32 {
	var acc uint32
	for p := int32(0); p < passes; p++ {
		var s uint32
		for _, w := range words {
			s += w
		}
		acc += s
	}
	return acc
}

// prng: xorshift32. No memory at all, so it isolates arithmetic lowering --
// shifts and xor, which is where FkLua already beats idiomatic Lua because
// bit32 is a function call per operation.
//
//go:wasmexport pure_prng
func purePrng(n int32) uint32 {
	x := uint32(2463534242)
	for i := int32(0); i < n; i++ {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
	}
	return x
}

var fa, fb []float64

//go:wasmexport dot_setup
func dotSetup(n int32) {
	fa = make([]float64, n)
	fb = make([]float64, n)
	for i := range fa {
		fa[i] = float64(i) * 0.5
		fb[i] = float64(i)*0.25 + 1.0
	}
}

// dot: an f64 dot product. Every element is a double in linear memory, which
// FkLua reassembles from two words -- the kernel that most exposes the cost of
// not having a native float array.
//
//go:wasmexport pure_dot
func pureDot(passes int32) float64 {
	var acc float64
	for p := int32(0); p < passes; p++ {
		var s float64
		for i := range fa {
			s += fa[i] * fb[i]
		}
		acc += s
	}
	return acc
}

// ---------------------------------------------------------------------------
// REALISTIC kernels: what a Factorio mod inner loop actually does.
//
// These bring the guest's RUNTIME into the comparison -- structs, bounds
// checks, hashing, allocation, string building -- against Lua primitives that
// are C functions. That is a much harder comparison for a compiled guest, and
// it is the one a mod author is really making.
// ---------------------------------------------------------------------------

// An entity record, the shape a mod carries per machine it tracks.
type entity struct {
	kind    uint8
	active  bool
	x, y    int32
	amount  uint32
	quality uint16
}

var ents []entity

//go:wasmexport ents_setup
func entsSetup(n int32) {
	ents = make([]entity, n)
	for i := range ents {
		ents[i] = entity{
			kind:    uint8(i % 7),
			active:  i%3 != 0,
			x:       int32(i%512) - 256,
			y:       int32(i/512) - 256,
			amount:  uint32(i) * 2654435761 % 1000,
			quality: uint16(i % 5),
		}
	}
}

// entities: scan a struct array, filter, and aggregate per kind.
//
// This is the single most common shape in a real mod -- walk the things you
// track, skip the ones that do not apply, total what is left. It is a struct
// FIELD access pattern rather than a flat array one, so it exercises the offset
// arithmetic and the sub-word loads a struct layout produces.
//
//go:wasmexport real_entities
func realEntities(passes int32) uint32 {
	var acc uint32
	for p := int32(0); p < passes; p++ {
		var totals [7]uint32
		for i := range ents {
			e := &ents[i]
			if !e.active || e.quality == 0 {
				continue
			}
			if e.x < -128 || e.x > 128 {
				continue
			}
			totals[e.kind] += e.amount
		}
		for _, t := range totals {
			acc = acc*31 + t
		}
	}
	return acc
}

var grid []uint8

//go:wasmexport grid_setup
func gridSetup(side int32) {
	grid = make([]uint8, side*side)
	for i := range grid {
		// A deterministic pseudo-random maze, ~30% walls.
		h := uint32(i) * 2654435761
		if (h>>16)%10 < 3 {
			grid[i] = 1
		}
	}
}

// grid: a flood fill over a 2D grid, with an explicit stack.
//
// This is belt and pipe network traversal, the other shape mods spend real time
// in. It is branch-heavy and its memory access is scattered rather than
// sequential, so it defeats the sequential-access assumptions the sum kernel
// rewards.
//
//go:wasmexport real_grid
func realGrid(side int32, passes int32) uint32 {
	var acc uint32
	seen := make([]uint8, side*side)
	stack := make([]int32, 0, side*side)
	for p := int32(0); p < passes; p++ {
		for i := range seen {
			seen[i] = 0
		}
		stack = stack[:0]
		var filled uint32
		// The first open cell at or after the centre. Starting AT the centre
		// is what the first version did, and on this maze the centre is a
		// wall -- so the fill visited nothing and every language agreed on a
		// checksum of zero. A benchmark that agrees about doing no work is
		// still a benchmark that does no work.
		start := (side/2)*side + side/2
		for start < side*side && grid[start] != 0 {
			start++
		}
		if start < side*side {
			stack = append(stack, start)
			seen[start] = 1
		}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			filled++
			cx, cy := cur%side, cur/side
			// Four neighbours, bounds-checked against the grid edges.
			if cx > 0 {
				n := cur - 1
				if grid[n] == 0 && seen[n] == 0 {
					seen[n] = 1
					stack = append(stack, n)
				}
			}
			if cx < side-1 {
				n := cur + 1
				if grid[n] == 0 && seen[n] == 0 {
					seen[n] = 1
					stack = append(stack, n)
				}
			}
			if cy > 0 {
				n := cur - side
				if grid[n] == 0 && seen[n] == 0 {
					seen[n] = 1
					stack = append(stack, n)
				}
			}
			if cy < side-1 {
				n := cur + side
				if grid[n] == 0 && seen[n] == 0 {
					seen[n] = 1
					stack = append(stack, n)
				}
			}
		}
		acc = acc*31 + filled
	}
	return acc
}

// names: build and hash prototype-name strings.
//
// This is the kernel a compiled guest should LOSE, and it is here for exactly
// that reason. Lua strings are C-implemented, interned and hashed by the
// runtime; a guest builds them byte by byte in linear memory with its own
// allocator. A benchmark suite that only contained the kernels FkLua wins would
// not be telling anyone the truth about what to write in what.
//
//go:wasmexport real_names
func realNames(n int32) uint32 {
	var acc uint32
	const digits = "0123456789"
	for i := int32(0); i < n; i++ {
		// "iron-plate-<i>", built the way guest code would.
		buf := make([]byte, 0, 24)
		buf = append(buf, "iron-plate-"...)
		v := i
		if v == 0 {
			buf = append(buf, '0')
		} else {
			var tmp [10]byte
			k := 0
			for v > 0 {
				tmp[k] = digits[v%10]
				v /= 10
				k++
			}
			for k > 0 {
				k--
				buf = append(buf, tmp[k])
			}
		}
		// FNV-1a over the result, so the string is actually consumed.
		h := uint32(2166136261)
		for _, c := range buf {
			h ^= uint32(c)
			h *= 16777619
		}
		acc = acc*31 + h
	}
	return acc
}
