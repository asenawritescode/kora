//go:build !linux

package qjs

import (
	"github.com/tetratelabs/wazero/experimental"
)

// Copy-on-write memory needs memfd and mmap, which only linux provides here. Off linux
// the engine never allocates cow memory, so every snapshot is copied on restore and reset.

type cowImage struct{}

func newCowImage([]byte) (*cowImage, error) { return nil, errNoCOW }

func (*cowImage) Close() {}

func probeCOW() bool { return false }

type cowAllocator struct {
	mem *cowMemory
}

// Allocate is never reached: the engine only installs this allocator when probeCOW holds.
func (*cowAllocator) Allocate(uint64, uint64) experimental.LinearMemory { return nil }

type cowMemory struct{}

func (*cowMemory) mapImage(*cowImage, int) error { return errNoCOW }
