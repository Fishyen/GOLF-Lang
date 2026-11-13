// Copyright 2026 The GOLF Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package golf

// Copy and Transfer make cross-mode ownership explicit at a call
// boundary between a gc-mode file and a manual-mode file (see
// DESIGN.md §3.3). The compiler requires any pointer-typed argument
// passed across such a boundary to be wrapped in one of these two
// calls; passing a bare pointer across the boundary is a compile
// error.
//
// Copy is the default-shaped choice: it makes a shallow copy of the
// pointee and passes the copy's address, so the caller and callee
// each own an independent value afterward.
//
// Transfer is the explicit escape hatch for zero-copy ownership
// handoff: it passes p through unchanged. Using it is a statement
// that the caller gives up the pointer -- p must not be read or
// written by the caller after the call.
//
// Neither function changes which heap (GC-managed or golf.Alloc'd)
// the pointee lives in; that is the caller's responsibility.

// Copy returns a pointer to a new value holding a shallow copy of *p.
func Copy[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// Transfer returns p unchanged. It exists only to mark, at the call
// site, that ownership of *p is being handed off rather than copied.
func Transfer[T any](p *T) *T {
	return p
}

// CopySlice and TransferSlice are Copy and Transfer's equivalents for
// slice-typed values, which hold an implicit reference to a backing
// array and so carry the same cross-mode hazard as a bare pointer.

// CopySlice returns a new slice holding a shallow copy of s's
// elements, backed by a new array s does not share.
func CopySlice[T any](s []T) []T {
	if s == nil {
		return nil
	}
	c := make([]T, len(s))
	copy(c, s)
	return c
}

// TransferSlice returns s unchanged. It exists only to mark, at the
// call site, that ownership of s's backing array is being handed off
// rather than copied.
func TransferSlice[T any](s []T) []T {
	return s
}
