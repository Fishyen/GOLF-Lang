# GOLF — Design Notes (v0 brainstorm)

**GOLF** ("Go's Outer Limits Fixed") is a fork of Go targeting performance-heavy
application development (e.g. large-scale games) without giving up Go's
conveniences where they don't cost correctness or speed.

This document captures the basics agreed on so far. It is a starting point,
not a locked spec — details (especially syntax) are placeholders.

## 1. Goals

- Fix Go's performance ceiling for CPU/memory-sensitive software, while
  keeping Go's ergonomics as the default experience.
- Let a single program mix memory-management strategies across files, so a
  renderer can be as fast as hand-tuned manual code while the rest of the
  game keeps GC convenience.
- Reduce compiled output size by only embedding the parts of a
  library/module a program actually uses, rather than the whole package.
- General compiler/codegen optimizations beyond what `go build` does today.
- QOL additions the Go community commonly wants, without turning into a
  different language in the common case.

## 2. Selective linking (library embedding)

The compiler should only emit the functions/types actually reachable from a
program's call graph, rather than linking a whole package. This is a
transitive reachability analysis over module boundaries. Not expanded on
yet beyond being a stated goal — revisit once the memory-mode system is
settled, since import handling may interact with per-file mode metadata.

## 3. Memory management modes

### 3.1 Supported modes (v1)

- **GC** — Go's existing tracing garbage collector. Default mode.
- **Manual** — explicit allocation/free, no GC involvement.
- *(Deferred)* **RSR** ("Real Scoped Regions") — a future third mode, not
  implemented in v1. The mode directive's syntax must leave room for a third
  value later without a breaking change.

### 3.2 Granularity: file-level mode + per-allocation opt-out

- The primary unit of mode selection is the **file**: a compiler directive
  at the top of a file sets its default memory mode (`gc` or `manual`).
  Different files in the same program can use different modes; everything
  still links into one executable.
- Within a file, **individual allocations can opt out of the file's default
  mode** via a per-allocation modifier (e.g. annotating a specific
  allocation site or variable declaration). This covers the realistic case
  of a mostly-manual renderer file that still wants one or two GC-backed
  caches, without forcing a whole second file just for that.
- Exact syntax is TBD for both the file-level directive and the
  per-allocation modifier. Suggested shape to keep RSR pluggable later:
  `//golf:mem(gc|manual)` at file scope, with an equivalent inline
  form/annotation at the declaration/allocation site for the opt-out case.

### 3.3 Cross-mode ownership: copy at the boundary

When a value crosses from one mode's code into another's (e.g. a GC file
passes a slice into a manual-mode function, or vice versa), the boundary
**copies the value** rather than implicitly transferring ownership.

- Rationale: implicit ownership-mode mutation on a variable — where the same
  variable's deallocation contract silently changes depending on dataflow
  history — is a correctness and readability hazard, especially in large
  codebases where "who frees this" needs to be locally obvious.
- An explicit **transfer** construct (exact keyword/syntax TBD) is the
  escape hatch for cases that genuinely want zero-copy ownership handoff
  instead of a copy. This is opt-in, not the default.
- This applies in both directions (GC→manual and manual→GC copies are both
  real cases — e.g. a manual renderer handing a result back up to GC-managed
  game logic).

**Status:** implemented for direct pointer-typed call arguments.
`golf.Copy[T](*T) *T` and `golf.Transfer[T](*T) *T` (package `golf`, see
§3.5) are the "copy" and "transfer" primitives. The compiler now rejects a
call where the caller's file mode and the callee's declaring-file mode
differ (checked only within the same package for v1) and a pointer-typed
argument is passed bare -- it must be the direct result of a `golf.Copy(…)`
or `golf.Transfer(…)` call instead:

```go
//golf:mem(manual)
package renderer

func Render(p *Point) { ... }
```

```go
package game
import "golf"

func update() {
    p := &Point{X: 1, Y: 2}
    renderer.Render(golf.Copy(p))      // ok: explicit copy
    renderer.Render(golf.Transfer(p))  // ok: explicit handoff; p must not
                                        // be used by this file again
    renderer.Render(p)                 // compile error: bare pointer
}
```

**Not yet done (named gaps, not solved by v1):**
- Only direct pointer arguments at call sites are checked. Slices, and
  struct fields that themselves hold pointers, cross boundaries unchecked
  -- the same hazard noted in §3.5 for `golf.Alloc`.
- Return values are not checked: a manual-mode function returning a
  pointer to a GC-mode caller (or vice versa) is not yet caught.
- The check only looks at calls within the same package; crossing a
  package boundary into a differently-moded file in another package isn't
  detected.
- `golf.Alloc`/`golf.Free` are still not gated to manual-mode files (see
  §3.5) -- this was a deliberate decision to preserve interoperability,
  not an oversight. `golf.Copy`/`golf.Transfer` are plain generic functions
  usable from any file for the same reason.

### 3.4 Manual mode is a stricter dialect

Manual mode is not just "GC mode plus malloc/free." Several Go features
assume a GC and don't have an obvious manual-mode equivalent yet:

- Closures capturing variables implicitly (capture-by-reference assumes GC
  keeps captured data alive).
- Interface values, which can trigger heap escape under the hood.
- Built-in `map`, whose implementation assumes GC-managed backing storage.

For v1, manual-mode files should be expected to work with a **smaller
language surface** than GC-mode files — closer to "C with Go syntax" for
anything that would otherwise rely on hidden GC support. This is an accepted
tradeoff for v1, not something to paper over with hidden allocations.

**Status:** all three restrictions (map, closures, interfaces) are enforced
by the compiler as of this writing -- a `//golf:mem(manual)` file that uses
any of them fails to compile.

### 3.5 Manual allocator (`golf.Alloc`/`golf.Free`)

The `golf` package (`src/golf/alloc.go`) provides `Alloc[T]() *T` and
`Free[T](*T)`, backed by `mmap`/`munmap` directly -- memory that Go's GC
never scans or tracks. This is the allocator manual-mode code is meant to
use.

**Decision:** this package is not gated to manual-mode files, and will stay
that way -- any file may import and call it. Restricting it would hurt
interoperability (e.g. a GC-mode file that wants one manually-managed
buffer) for no correctness benefit once §3.3's copy-at-boundary check
covers pointer arguments crossing modes; it simply isn't *necessary* in a
gc-mode file, not forbidden.

**Open hazard, not solved by v1:** nothing currently stops a program from
storing a GC-managed value (a string, slice, pointer into the Go heap --
maps/interfaces are separately disallowed per §3.4) inside memory obtained
from `golf.Alloc`. Since the GC doesn't scan that memory, such a value's
referent can be collected while still referenced from inside the manual
allocation, corrupting the program. This is the same hazard malloc'd memory
has in any GC'd language; this document doesn't yet have an answer for it
(a possible future direction: extend the manual-mode restrictions to forbid
storing GC-tracked pointer kinds in manual-mode struct fields, but this
isn't designed).

## 4. Open questions / not yet decided

- Exact directive syntax (file-level and per-allocation).
- How selective linking's reachability analysis interacts with per-file
  memory-mode metadata.
- RSR design itself (explicitly deferred past v1).
- The rest of the QOL/general-optimization feature list — not yet
  enumerated beyond "general optimizations" and "QOL additions."
