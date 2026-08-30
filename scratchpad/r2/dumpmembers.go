//go:build ignore

// dumpmembers prints the SHIPPED member table for one member id, so the
// benchmark beside it measures the real layout rather than one written out by
// hand.
//
// The whole point of the harness this feeds is that both the tier-2 and the
// typed decode go through the real runtime/lua/fk_abi.lua against the real
// generated `sig`, so a hand-written stand-in for either would measure a
// different program from the one that ships.
//
//	go run scratchpad/r2/dumpmembers.go 1932 > testdata/tmp/r2/members.lua
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Techrocket9/fklua/internal/factorio"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: dumpmembers.go ID...")
		os.Exit(2)
	}
	root, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	a, err := factorio.LoadAPI(filepath.Join(root, "api",
		factorio.DefaultAPIVersion, "runtime-api.json"))
	if err != nil {
		panic(err)
	}
	ids := map[int]bool{}
	for _, s := range os.Args[1:] {
		n, err := strconv.Atoi(s)
		if err != nil {
			panic(err)
		}
		ids[n] = true
	}
	r := factorio.GenerateMembers(a)
	src, err := r.Only(ids).LuaSourceWith(a, factorio.EventReport{})
	if err != nil {
		panic(err)
	}
	fmt.Print(src)
}
