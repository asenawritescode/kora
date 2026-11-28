//go:build linux

package qjs

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"github.com/tetratelabs/wazero/experimental"
	"golang.org/x/sys/unix"
)

// cowImage holds one snapshot's bytes in an anonymous memfd. Every instance restored
// from the snapshot maps it MAP_PRIVATE, so pages are shared until the instance writes
// them and a reset is one mmap instead of a copy of the whole memory.
type cowImage struct {
	fd   int // -1 once closed
	size int
}

// newCowImage copies mem into a new memfd.
func newCowImage(mem []byte) (*cowImage, error) {
	fd, err := unix.MemfdCreate("kora-qjs-snapshot", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("memfd_create: %w", err)
	}
	img := &cowImage{fd: fd, size: len(mem)}
	if err := unix.Ftruncate(fd, int64(len(mem))); err != nil {
		img.Close()
		return nil, fmt.Errorf("ftruncate memfd: %w", err)
	}
	for off := 0; off < len(mem); {
		n, err := unix.Pwrite(fd, mem[off:], int64(off))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || n == 0 {
			img.Close()
			return nil, fmt.Errorf("write memfd at %d: %w", off, err)
		}
		off += n
	}
	return img, nil
}

// Close releases the memfd. Mappings made from it stay valid until they are unmapped.
func (c *cowImage) Close() {
	if c == nil {
		return
	}
	if c.fd >= 0 {
		_ = unix.Close(c.fd) // best effort: nothing to recover once the memfd is closed
		c.fd = -1
	}
}

// probeCOW reports whether this process can build copy-on-write memory: a memfd can be
// created and a PROT_NONE reservation mapped. It runs once per engine.
func probeCOW() bool {
	img, err := newCowImage(make([]byte, wasmPageBytes))
	if err != nil {
		return false
	}
	defer img.Close()
	res, err := unix.Mmap(-1, 0, wasmPageBytes, unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANON|unix.MAP_NORESERVE)
	if err != nil {
		return false
	}
	return unix.Munmap(res) == nil
}

// cowAllocator is the wazero memory allocator for one module instantiation. It records
// the memory it allocates, because wazero does not expose its LinearMemory.
type cowAllocator struct {
	mem *cowMemory
}

// Allocate reserves maxBytes of address space for the memory. The address never moves:
// growing only makes more of the reservation accessible. A failed reservation falls back
// to a Go-heap buffer, which restores copy-reset (see mapImage).
func (a *cowAllocator) Allocate(_, maxBytes uint64) experimental.LinearMemory {
	m := &cowMemory{}
	if res, err := unix.Mmap(-1, 0, int(maxBytes), unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANON|unix.MAP_NORESERVE); err == nil {
		m.reserved = res
	}
	a.mem = m
	return m
}

// cowMemory backs one wasm linear memory. reserved is the whole reservation; the first
// accessible bytes are readable and writable. When reserved is nil the memory lives in
// heap, the fallback for a failed reservation.
type cowMemory struct {
	reserved   []byte
	heap       []byte
	accessible int
	mapped     int // successful mapImage calls, so tests can tell mapping from copying
}

// Reallocate makes size bytes accessible and returns them. Wazero never shrinks memory.
func (c *cowMemory) Reallocate(size uint64) []byte {
	if c.reserved == nil {
		return c.reallocHeap(size)
	}
	if size > uint64(len(c.reserved)) {
		return nil
	}
	if n := int(size); n > c.accessible {
		if err := unix.Mprotect(c.reserved[c.accessible:n], unix.PROT_READ|unix.PROT_WRITE); err != nil {
			return nil
		}
		c.accessible = n
	}
	return c.reserved[:size:len(c.reserved)]
}

// reallocHeap grows the heap fallback, keeping its contents.
func (c *cowMemory) reallocHeap(size uint64) []byte {
	if uint64(len(c.heap)) >= size {
		return c.heap[:size]
	}
	grown := make([]byte, size)
	copy(grown, c.heap)
	c.heap = grown
	return grown
}

// Free unmaps the reservation. The instance is closed after this.
func (c *cowMemory) Free() {
	if c.reserved != nil {
		_ = unix.Munmap(c.reserved) // best effort: the mapping is released with the process at worst
		c.reserved = nil
		c.accessible = 0
	}
}

// mapImage makes bytes [0, size) of the memory a private view of img, discarding every
// page the instance wrote, and zeroes the accessible pages above size. It fails for the
// heap fallback, so the caller copies the snapshot instead. An error leaves the memory
// in an unknown state: the instance must be closed.
func (c *cowMemory) mapImage(img *cowImage, size int) error {
	if c == nil || c.reserved == nil || img == nil || size > c.accessible {
		return errNoCOW
	}
	if img.fd < 0 {
		return errors.New("qjs: snapshot image is closed")
	}
	// The address of our own reservation, handed back to mmap; MAP_FIXED keeps it.
	base := uintptr(unsafe.Pointer(&c.reserved[0])) //nolint:gosec // G103: our mapping's address, not a Go pointer kept past the call
	const prot = unix.PROT_READ | unix.PROT_WRITE
	_, _, errno := syscall.Syscall6(syscall.SYS_MMAP, base, uintptr(size), prot,
		unix.MAP_FIXED|unix.MAP_PRIVATE, uintptr(img.fd), 0)
	if errno != 0 {
		return fmt.Errorf("mmap snapshot: %w", errno)
	}
	c.mapped++
	if c.accessible > size {
		if err := unix.Madvise(c.reserved[size:c.accessible], unix.MADV_DONTNEED); err != nil {
			return fmt.Errorf("madvise above snapshot: %w", err)
		}
	}
	return nil
}
