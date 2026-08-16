//go:build !growbig

package main

// One megabyte, taken 64 KiB at a time. This is the default because the
// round-trip runs this guest on every invocation and the point of the leg is
// that a grow happens at all, not how large it is.
const (
	blockSize = 64 << 10
	blocks    = 16
)
