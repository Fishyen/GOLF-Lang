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
program's call graph, rather than linking a whole package.

**Investigated (2026-11-13), before building anything:** this goal is
already substantially met by Go's stock linker, inherited as-is by GOLF.
Empirically (via `go tool nm` / `strings` on built binaries, inlining
disabled to get a clean signal):
- An unused free function in an imported package is fully eliminated,
  including any string literals or calls it alone was responsible for.
- An unused *type* (never constructed anywhere reachable) is eliminated
  entirely, methods included.
- A *used* type's unused methods are eliminated individually -- even when
  the type is stored in an interface alongside other implementations, and
  even with `reflect` imported and used elsewhere in the program (e.g.
  `reflect.TypeOf`, or `reflect.Value.MethodByName` called on an
  *unrelated* type, even with a non-constant method-name string).

**The one real gap found:** when a value of type `T` itself flows into a
`reflect.Value` (e.g. `reflect.ValueOf(t)`) anywhere reachable, and that
`reflect.Value` is used for *dynamic* method lookup (`MethodByName` with a
non-constant string -- a constant string lets the linker resolve it
statically, same as a direct call), the linker falls back to retaining
**every exported method of `T`**, whether or not that specific method is
ever actually invoked. This is the realistic case that matters for a
game: a reflection-driven save system, network RPC dispatcher, or
scripting/ECS binding that walks a struct's methods by name pulls in the
whole method set of every type it ever touches, not just the methods
actually exercised at runtime.

**Revised scope:** "selective linking" for GOLF is not "build a
reachability analysis from scratch" -- duplicating a solid pass the
linker already does well -- but closing this one specific gap: giving the
programmer a way to tell the compiler/linker exactly which methods a
reflection-driven caller actually needs for a given type, so the rest of
that type's exported methods stay eligible for the dead-code elimination
they'd otherwise lose.

**Chosen mechanism:** `//golf:noreflect(Method, ...)` immediately before a
type declaration -- an *exemption* list, not an allow-list: it names
specific exported methods of that type the programmer asserts are never
looked up dynamically (`reflect.Value.MethodByName`/`Type.MethodByName`
with a non-constant name), so cmd/link's deadcode pass can treat them as
ordinarily dead-code-eligible even though the type's *rtype is reachable
and some reflect method lookup elsewhere in the binary would otherwise
force retaining its entire method set (§2's investigation). Everything
else about the type is unaffected; omit the directive and nothing
changes.

This spans three separate subsystems that have to stay consistent --
frontend directive parsing, the method-table encoding `cmd/compile`'s
`reflectdata` package emits per type, and `cmd/link`'s deadcode pass that
reads it -- so it was built in two steps, the same "inert plumbing first"
pattern §3's mode directive started with.

**Status (step 1, done):** frontend parsing and validation only, with no
effect on compiled output yet -- a correctness increment, not a
performance one. `//golf:noreflect(...)` is recognized only immediately
before a type declaration (erroring as misplaced anywhere else, including
the package clause or a func/var decl); each named method must actually
exist on that type and be exported (anything else errors, including a
duplicate name in the list); a valid directive's method list is recorded
in `pkgWriter.typeNoReflectMethods` and, with the new `-d=golfnoreflect`
debug flag, printed per type -- but nothing downstream (codegen, the
linker) reads that map yet, so right now the directive changes nothing
about what gets compiled or linked.

**Not yet done:** step 2 -- actually wiring the validated list through to
`cmd/link`'s deadcode pass (`src/cmd/link/internal/ld/deadcode.go`), so a
listed method is skipped when the pass would otherwise retain a
reflect-touched type's whole method set. The natural implementation
(researched, not yet built) avoids changing `cmd/internal/goobj`'s
per-symbol flag format, which is already at capacity (`Sym.Flag`/`Flag2`
are full 8-bit fields -- widening either would shift every subsequent
field's byte offset for every symbol in every object file, a far
higher-risk change than anything built so far): instead, `reflectdata`
would emit a plain auxiliary data symbol per annotated type, named by a
predictable convention off that type's existing `*rtype` descriptor
symbol name, holding the exempt method names; `deadcode.go`'s retention
loop would probe for that symbol by name (via the type symbol it already
has, `methodref.src`) and skip any method whose name appears in it. This
needs real verification before it can be trusted -- the stdlib test suite
doesn't exercise this new directive at all, so a subtle bug in it (a
method wrongly dropped) would only surface as a runtime failure in actual
annotated reflection code, not a build break, which is a weaker safety
net than everything else built so far this session had.

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

A single-result cross-mode call is checked the same way at the use site: a
bare `p := MakePoint()` where `MakePoint` crosses modes is rejected unless
written `p := golf.Copy(MakePoint())` (or `golf.Transfer(...)`).

**Status update:** slices are now checked the same way as pointers, via
`golf.CopySlice[T]`/`golf.TransferSlice[T]` (same package, same
copy-by-default/opt-in-transfer shape) -- a slice argument or single-result
slice return crossing a mode boundary must be wrapped just like a pointer
would be. A cross-mode call with more than one result, where any result is
a pointer or a slice, is now a hard compile error (`golf.Copy`/`Transfer`
only take one value, so there is no syntax yet to make a multi-result
crossing explicit) rather than a silent gap -- it must be restructured
into single-result calls to cross the boundary at all.

**Status update:** a struct (or array) that itself has no top-level
indirection but contains a pointer or slice field somewhere inside it --
at any depth, recursively -- is now also checked, via
`golf.CopyValue[T]`/`golf.TransferValue[T]`. Unlike `Copy`/`CopySlice`,
which only copy one level, `CopyValue` copies *recursively*: every
pointer or slice reachable from the value, through any number of nested
structs/slices/arrays, gets its own fresh copy (implemented with
`reflect`, including unexported fields via `unsafe`, since there's no way
to wrap an individual inner field at the call site the way there is for a
bare pointer argument). A plain struct with no indirection anywhere inside
it (e.g. `struct{ A, B int }`) is unaffected -- Go's by-value call
semantics already make it safe. Recursive type definitions (e.g. a linked
list's `Node{Next *Node}`) classify fine and don't risk infinite
recursion, since the check never follows a pointer into its pointee, only
drills into a struct's or array's *own* fields/elements.

**Status update:** the check now also covers calls into *imported*
packages, not just within the same package. A function's `//golf:mem`
mode is written into the public per-object record every exported
function/method already carries in GOLF's compiled export data (right
after its declaration position, before `funcExt`'s private extension
data) -- the same record a `go/types2`-style importer decodes to
reconstruct an imported package's types for the importing package's own
type-checking. `cmd/compile/internal/testimporter` (GOLF's `go/types2`
importer) reads it back and exposes it via `testimporter.FuncMode(f)`,
keyed by the `*types2.Func` it constructs for each imported function;
`calleeGolfMode` in writer.go checks `pkgWriter.funcGolfMode` (current
package) first and falls back to `testimporter.FuncMode` (imported
package) transparently, so every check already built on top of it --
arguments, returns, slices, structs -- now works across a package
boundary exactly the same way it does within one.

This touches the one piece of shared, high-blast-radius machinery in this
whole feature: the per-object record format that *every* exported
declaration in *every* package goes through, decoded by two independent
readers (`noder/reader.go` for re-reading a package's own declarations,
`testimporter/ureader.go` for importing another package's). Both were
updated to consume the new field in lockstep with the writer. Verified
with: a full `./make.bash` three-stage bootstrap (the compiler compiling
itself with the new format, at every stage); `go test` across the whole
of `cmd/compile/...` (every subpackage, no failures); `go build std cmd`
and `go vet std` (clean); `go test` on `reflect`, `go/types`,
`go/importer`, plus the usual encoding/json, net/http, errors, fmt
spot-check (all ok); and a hand-built two-package example (a separately
`go build`-compiled `manual`-mode `renderer` package imported by a
`gc`-mode `main`) confirming a bare cross-package pointer argument and
return are both now rejected, wrapping them in `golf.Copy`/`Transfer`
fixes it, and an unrelated same-mode cross-package call is unaffected.

**Not yet done (named gap, not solved by v1):**
- Map- and interface-typed fields inside a crossing struct aren't
  detected or deep-copied. The main case this would matter for -- a
  manual-mode-declared struct smuggling one across -- can't arise, since
  manual-mode files can't declare map or interface types at all (§3.4);
  what's left unchecked is a GC-mode struct's map/interface field
  crossing into manual code.
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

**Benchmarked (2026-11-13), per-object `Alloc`/`Free` is a net loss, not a
win:** one `mmap`/`munmap` syscall per object is extremely expensive next to
Go's bump-allocating GC heap:

| pattern (n=1000 `Vec3{X,Y,Z float64}`)         | time/op   |
|-------------------------------------------------|-----------|
| `golf.Alloc`+`Free` per object, immediately     | 2.29 ms   |
| `new(Vec3)` per object, immediately (escape-analyzed away) | 138 ns |
| `golf.Alloc` n objects, keep, then `Free` all   | 2.70 ms   |
| `new(Vec3)` n objects, keep (real heap allocs)  | 9.68 µs   |
| `golf.AllocN[Vec3](n)` -- **one** mmap for all n, one `FreeN` | 6.72 µs |

Per-object manual allocation is ~16,000x slower than a stack-allocated GC
value and ~245x slower than a real GC-heap allocation of the same shape --
syscall overhead dominates completely. A single bulk allocation
(`golf.AllocN`/`FreeN`, added alongside this benchmark) amortizes that
overhead away and comes out *ahead* of the GC-heap case (6.7µs vs 9.7µs)
by skipping GC bookkeeping entirely once the syscall cost is shared across
the whole batch.

**Conclusion:** `golf.Alloc`/`Free` as implemented should not be reached
for repeated small allocations -- doing so is a regression, not an
optimization. The actual performance lever for manual-mode code is
amortizing the syscall over many objects.

### 3.6 Bulk allocation (`golf.AllocN`/`FreeN`, `golf.Arena`)

Following directly from §3.5's benchmark, `golf` provides two ways to
amortize allocation cost instead of paying a syscall per object:

- **`AllocN[T](n) []T` / `FreeN[T](s)`** -- one mmap/munmap for n
  contiguous `T`s, for the simple case of "I know I need n of these up
  front, and I'll free them all together."
- **`Arena`** (`NewArena`, `ArenaAlloc[T]`, `ArenaAllocN[T]`, `Reset`,
  `Free`) -- a bump allocator: it maps chunks as needed and hands out
  sub-allocations with no syscall per allocation, growing by mapping a
  new chunk (never moving existing memory, so previously returned
  pointers stay valid) rather than per-object `mmap`. `Reset` makes all
  mapped chunks available again with **no new syscalls** -- the intended
  pattern for something like a per-frame scratch allocator: allocate
  through the frame, `Reset` at the end, reuse next frame.

**Benchmarked (2026-11-13):** a per-frame pattern (allocate 1000 `Vec3`s
from a shared `Arena`, use them, `Reset`) runs in **538 ns/op, 0
allocs/op**, once warm -- about **19.5x faster** than the equivalent
`new(Vec3)`-per-object GC pattern (10.5 µs/op, 1000 allocs/op). This is
the actual performance win manual mode has been looking for: not
`Alloc`/`Free` per object, and not needed from RSR either -- `Arena`
already delivers it today, by turning "many small allocations with a
shared lifetime" into "one allocation, reused."

**Note:** `Arena` is *not* RSR. It has no scoped lifetimes, can't free a
single allocation out from the middle, and the whole arena goes away (or
resets) at once -- it's a general bulk-allocation tool, built ahead of
RSR because RSR's actual scope-lifetime design is more involved and will
be designed separately, later (§3.1). RSR is expected to build on the
same amortized-allocation idea, with real per-scope lifetime tracking on
top of something like this.

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
