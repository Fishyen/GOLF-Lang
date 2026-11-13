// Copyright 2026 The GOLF Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package golf

import "unsafe"

// Arena is a bump allocator: it hands out sub-allocations from
// mmap'd chunks it owns, without a syscall per allocation. It exists
// because per-object Alloc/Free is dominated by mmap/munmap overhead
// (see DESIGN.md §3.5's benchmark) -- Arena amortizes that cost over
// many allocations instead.
//
// Arena is a general bulk-allocation tool, not GOLF's planned RSR
// memory mode (DESIGN.md §3.1/RSR plan): it has no scoped lifetimes
// and does not track or validate individual allocations' extents.
// Values allocated from an Arena remain valid until Reset or Free is
// called on the Arena itself; there is no way to free a single
// allocation out of an Arena early. RSR, designed separately later,
// is expected to build on the same amortized-allocation idea with a
// real scope-based lifetime model on top.
//
// An Arena is not safe for concurrent use.
type Arena struct {
	chunks    [][]byte
	off       int // next free byte offset within the last chunk
	chunkSize int // size used for new chunks (rounded up to a page)
}

// NewArena creates an Arena whose chunks are sized at least
// chunkSize bytes (rounded up to a page). Larger individual
// allocations still succeed; they simply get their own
// larger-than-chunkSize chunk.
func NewArena(chunkSize int) *Arena {
	if chunkSize <= 0 {
		chunkSize = 1
	}
	a := &Arena{chunkSize: chunkSize}
	a.chunks = [][]byte{allocPages(chunkSize)}
	return a
}

// alignUp rounds off up to the next multiple of align (a power of two).
func alignUp(off, align int) int {
	return (off + align - 1) &^ (align - 1)
}

// allocBytes returns a pointer to size bytes aligned to align,
// bump-allocated from the arena's current chunk, mapping a fresh
// chunk first if the current one doesn't have room. Previously
// returned pointers are never invalidated by this.
func (a *Arena) allocBytes(size, align int) unsafe.Pointer {
	cur := a.chunks[len(a.chunks)-1]
	start := alignUp(a.off, align)
	if start+size > len(cur) {
		newSize := a.chunkSize
		if size > newSize {
			newSize = size
		}
		cur = allocPages(newSize)
		a.chunks = append(a.chunks, cur)
		start = 0
	}
	a.off = start + size
	return unsafe.Pointer(&cur[start])
}

// Reset makes all of the Arena's already-mapped chunks available for
// reuse again, without any new syscalls. Every pointer and slice
// previously returned by ArenaAlloc/ArenaAllocN for this Arena
// becomes invalid; using one after Reset is undefined behavior.
func (a *Arena) Reset() {
	a.off = 0
	if len(a.chunks) > 1 {
		// Keep only the first (largest-reused) chunk mapped; drop the
		// rest so repeated growth doesn't accumulate chunks forever.
		for _, c := range a.chunks[1:] {
			freePages(unsafe.Pointer(&c[0]), len(c))
		}
		a.chunks = a.chunks[:1]
	}
}

// Free releases every chunk the Arena has mapped. The Arena must not
// be used after Free returns.
func (a *Arena) Free() {
	for _, c := range a.chunks {
		freePages(unsafe.Pointer(&c[0]), len(c))
	}
	a.chunks = nil
}

// ArenaAlloc bump-allocates space for a single T from a and returns a
// pointer to it. The memory is not zeroed by the OS unless it comes
// from a freshly mapped chunk; a value already used and reset within
// this arena may contain stale bytes.
func ArenaAlloc[T any](a *Arena) *T {
	var zero T
	p := a.allocBytes(int(unsafe.Sizeof(zero)), int(unsafe.Alignof(zero)))
	return (*T)(p)
}

// ArenaAllocN bump-allocates space for n contiguous Ts from a and
// returns them as a slice.
func ArenaAllocN[T any](a *Arena, n int) []T {
	var zero T
	size := int(unsafe.Sizeof(zero)) * n
	if size == 0 {
		size = 1
	}
	p := a.allocBytes(size, int(unsafe.Alignof(zero)))
	return unsafe.Slice((*T)(p), n)
}
