//go:build gb40

package main

// 40 MiB: the size agents/guests.md's grow table reports a 288-365 ms tick at.
const targetBlocks = 40 * 1048576 / (blockWords * 4)
