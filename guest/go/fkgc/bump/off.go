//go:build !gc.custom

package bump

// HeapTop is 0 without the -gc=custom hooks; the file that defines the
// allocator is the same file that can answer this.
func HeapTop() uint32 { return 0 }
