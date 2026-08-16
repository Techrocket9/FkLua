//go:build !gb16 && !gb40

package main

// 4 MiB of retained blocks. A small guest, and the size at which sharding
// stage C measured a 22.7-30.0 ms grow tick.
const targetBlocks = 4 * 1048576 / (blockWords * 4)
