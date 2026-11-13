// Copyright 2026 The GOLF Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package golf

import (
	"reflect"
	"unsafe"
)

// CopyValue and TransferValue are Copy/Transfer's and
// CopySlice/TransferSlice's equivalent for a value whose type doesn't
// itself cross the boundary (DESIGN.md §3.3) through a single
// pointer or slice indirection, but through a pointer or slice field
// nested somewhere inside it -- a struct holding a cache *Entry or a
// []Item, for instance. The compiler requires such a value to be
// wrapped in one of these two when it's passed or returned across a
// gc-mode/manual-mode boundary, the same way it requires Copy/Transfer
// for a bare pointer.
//
// Unlike Copy, which only copies one level (the pointee itself, not
// anything *that* points to), CopyValue copies recursively: every
// pointer it finds -- at any depth, through any number of nested
// structs, slices, or arrays -- gets its own fresh copy, because
// there's no way to wrap an inner field individually at the call
// site. Map and interface-typed fields are not deep-copied (their
// backing data is still shared after CopyValue); see DESIGN.md §3.3.

// CopyValue returns a recursive copy of v: every pointer or slice v
// contains, at any depth, is replaced by a copy of what it points to,
// so v and the result share no mutable backing memory.
func CopyValue[T any](v T) T {
	orig := reflect.ValueOf(&v).Elem()
	return deepCopyValue(orig).Interface().(T)
}

// TransferValue returns v unchanged. It exists only to mark, at the
// call site, that ownership of everything v (transitively) points to
// is being handed off rather than copied.
func TransferValue[T any](v T) T {
	return v
}

// deepCopyValue returns a fresh copy of v. v may be derived from an
// unexported struct field (reflect then refuses v.Interface() and
// v.Set(...), but still allows navigating into it via Field/Index/
// Elem/Len/IsNil, which is all this function does with v itself); the
// value it returns is always freshly built via reflect.New or
// MakeSlice, so it's never subject to that restriction itself.
func deepCopyValue(v reflect.Value) reflect.Value {
	// reflect propagates a read-only flag through Field/Index/Elem
	// once a value has passed through an unexported field; that flag
	// blocks both v.Interface() and using v as the source of a Set,
	// not just setting v itself. Clear it up front via the same
	// unsafe trick setField uses for the destination side, so every
	// value this function touches or returns downstream is clean.
	if !v.CanInterface() {
		v = reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
	}

	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		setField(out.Elem(), deepCopyValue(v.Elem()))
		return out

	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		n := v.Len()
		out := reflect.MakeSlice(v.Type(), n, n)
		for i := 0; i < n; i++ {
			out.Index(i).Set(deepCopyValue(v.Index(i)))
		}
		return out

	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(deepCopyValue(v.Index(i)))
		}
		return out

	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			setField(out.Field(i), deepCopyValue(v.Field(i)))
		}
		return out

	default:
		// Scalars, maps, interfaces, chans, funcs: copied by value as
		// Go already would. Maps and interfaces still share their
		// backing data; see DESIGN.md §3.3's named gap.
		return v
	}
}

// setField sets dst to src, working around reflect's refusal to Set a
// Value obtained from an unexported struct field (dst is always one
// of deepCopyValue's own freshly built structs here, so the only
// reason CanSet could be false is dst itself being such a field).
func setField(dst, src reflect.Value) {
	if dst.CanSet() {
		dst.Set(src)
		return
	}
	reflect.NewAt(dst.Type(), unsafe.Pointer(dst.UnsafeAddr())).Elem().Set(src)
}
