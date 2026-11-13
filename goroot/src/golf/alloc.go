// Copyright 2026 The GOLF Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package golf provides GOLF's manual memory-management primitives.
//
// Alloc and Free allocate and release memory via the operating
// system (mmap/munmap), entirely outside Go's garbage-collected heap.
// The returned memory is never scanned or tracked by the GC.
//
// This is the allocator manual-mode files (//golf:mem(manual)) are
// meant to use. It is not yet restricted to manual-mode files only --
// that requires the viral cross-mode ownership rules described in
// DESIGN.md, which are not implemented yet.
//
// Caveat: since manual mode by itself can't (yet) prevent it, nothing
// stops a program from storing a GC-managed value (a string, slice,
// map, interface, or pointer into the Go heap) inside memory obtained
// from this package. The GC does not scan this memory, so any such
// value's referent can be collected out from under it, corrupting the
// program. This is the same hazard any malloc-backed allocation has
// when embedded in a garbage-collected runtime; this package does not
// attempt to detect or prevent it.
package golf

import (
	"syscall"
	"unsafe"
)

// Alloc allocates zeroed memory for a single T outside the Go heap
// and returns a pointer to it. The memory is not initialized by Go's
// runtime and is never garbage collected; it must be released with
// Free.
func Alloc[T any]() *T {
	b := allocPages(int(unsafe.Sizeof(*new(T))))
	return (*T)(unsafe.Pointer(&b[0]))
}

// Free releases memory previously returned by Alloc[T]. p must not be
// used after Free returns. Calling Free on a pointer not obtained
// from Alloc, or freeing the same pointer twice, is undefined
// behavior (the current implementation panics on a second free of the
// same pointer, since the OS rejects unmapping memory that is no
// longer mapped).
func Free[T any](p *T) {
	size := int(unsafe.Sizeof(*p))
	freePages(unsafe.Pointer(p), size)
}

// AllocN allocates zeroed memory for n contiguous Ts outside the Go
// heap in a single mapping and returns it as a slice. The memory is
// never garbage collected; it must be released with FreeN.
func AllocN[T any](n int) []T {
	var zero T
	size := int(unsafe.Sizeof(zero)) * n
	if size == 0 {
		size = 1
	}
	b := allocPages(size)
	return unsafe.Slice((*T)(unsafe.Pointer(&b[0])), n)
}

// FreeN releases memory previously returned by AllocN[T]. s must not
// be used after FreeN returns.
func FreeN[T any](s []T) {
	if len(s) == 0 {
		return
	}
	var zero T
	size := int(unsafe.Sizeof(zero)) * len(s)
	freePages(unsafe.Pointer(&s[0]), size)
}

func pageRoundUp(size int) int {
	pageSize := syscall.Getpagesize()
	if size <= 0 {
		size = 1
	}
	return ((size + pageSize - 1) / pageSize) * pageSize
}

func allocPages(size int) []byte {
	n := pageRoundUp(size)
	b, err := syscall.Mmap(-1, 0, n, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		panic("golf: Alloc: " + err.Error())
	}
	return b
}

func freePages(p unsafe.Pointer, size int) {
	n := pageRoundUp(size)
	b := unsafe.Slice((*byte)(p), n)
	if err := syscall.Munmap(b); err != nil {
		panic("golf: Free: " + err.Error())
	}
}
