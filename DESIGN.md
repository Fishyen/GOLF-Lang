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

**Status (step 2, done): enforced.** `//golf:noreflect` now actually
closes the gap end-to-end -- a named method of an annotated type is
dropped from the binary exactly as if reflection never touched that type,
even when some other dynamic `MethodByName` call elsewhere forces
retention of everything else.

Implementation, exactly as sketched above (avoiding `cmd/internal/goobj`'s
full `Sym.Flag`/`Flag2` bytes entirely):

- `types.Type` gained `GolfNoReflectMethods()`/`SetGolfNoReflectMethods()`,
  backed by a package-level `map[*Type][]string]` rather than a field on
  `Type` itself -- `Type` is allocated for every type the compiler ever
  touches, so a directive only a handful of types will ever use doesn't
  belong on the hot struct (a first attempt that added the field directly
  grew `unsafe.Sizeof(types.Type)` from 96 to 120 bytes and tripped
  `cmd/compile/internal/types`' own `TestSizeof`).
- The validated list travels from noder's `pkgWriter.typeNoReflectMethods`
  (keyed by `*types2.TypeName`) to that map (keyed by the eventual
  `*types.Type`) through the same per-object pkgbits record used for the
  cross-package mode lookup in §3.3 -- `w.Strings(...)` in `doObj`'s
  `*types2.TypeName` case, decoded back in `reader.go`'s `ObjType` case
  (used for both local and cross-package type construction) with
  `typ.SetGolfNoReflectMethods(names)`.
- `reflectdata.writeGolfNoReflectMethods`, called from `dextratype`
  (where a type's method table is written), emits the list as a plain
  rodata symbol named `"type:.golfnoreflect." + t.LinkString()` -- a
  DUPOK symbol found *by name*, not by relocation, since `cmd/link`'s
  deadcode pass needs no reachability graph edge to it, only to look it
  up given a type symbol it already has.
- `cmd/link/internal/ld/deadcode.go`'s retention loop now checks
  `!d.golfExempt(m)` before applying the `reflectSeen` fallback for a
  candidate method `m`; `golfExempt` computes the same name from `m.src`
  (`methodref`'s receiver type symbol) and looks up the aux symbol via
  `Loader.Lookup`, caching the decode per type symbol. This only affects
  the `reflectSeen` fallback branch -- a method still reachable through
  an actually-matched interface (`d.ifaceMethod`) or generic interface
  call (`d.genericIfaceMethod`) is retained regardless, since the
  directive asserts "never reached via reflection," not "never reachable
  at all."
- `*T`'s own type descriptor gets a *separate* `methodref`/probe from
  `T`'s (confirmed by instrumenting `golfExempt` directly during a real
  build and inspecting its output), since pointer-receiver method tables
  are emitted independently -- both `reflectdata.writeGolfNoReflectMethods`
  and `deadcode.go`'s `golfNoReflectSymName` resolve to `T`'s own
  (non-pointer) symbol name on either side, so one emitted aux symbol
  covers both.
- `testimporter/ureader.go`'s own, unrelated `ObjType` reading (for
  types2-level cross-package type-checking, not used by any of the above)
  needed a matching `_ = r.Strings()` added at the exact point the new
  write falls in the shared record, to stay byte-aligned with it; missing
  this the first time broke `TestImportedMethodOrder` by desyncing
  everything read after it for generic methods specifically.

**Verified:** beyond the usual full `./make.bash` three-stage bootstrap,
`go test` across `cmd/compile/...` (including `cmd/compile/internal/types2`
and `cmd/compile/internal/types`, both of which caught real regressions
during development -- the method-order desync and the `Type` size growth,
respectively -- before the stdlib build/vet pass would have), `go build
std cmd`, `go vet std`, and a stdlib spot-check (`reflect`, `go/types`,
`go/importer`, `encoding/json`, `net/http`, `errors`, `fmt`, `sort`,
`strings`) -- all clean. Then the actual empirical fix, end to end: a
type with one method genuinely looked up via `reflect.Value.MethodByName`
(a non-constant name) and a second, `//golf:noreflect`-exempted method
that is never looked up that way. Built and *run*: the genuinely-reflected
method resolves and calls correctly; `go tool nm` and `strings` on the
binary confirm the exempted method's function body is completely absent,
while an equivalent program with no directive at all keeps both methods,
exactly matching §2's original investigation.

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

**Status: per-allocation opt-out implemented, with dataflow tracking.** The
modifier is a real contextual keyword immediately preceding `var` in a
declaration (package-level or local, grouped or not) -- not a pragma
comment: `collect var x = ...` opts that declaration into GC mode, `manual
var x = ...` opts it into manual mode, and `real var x = ...` is reserved
for RSR (see §3.1) -- parsed now, but rejected with a clear "not
implemented yet" error at every use, since RSR itself doesn't exist yet.
There's no separate third file-level `//golf:mem(real)` mode to design
around yet either; only the per-declaration keyword exists so far.

```go
//golf:mem(manual)
package renderer

collect var frameCache = map[string]int{} // GC-backed, despite the file default

func fill() {
    frameCache["frames"] = 0 // fine: this assignment follows the override too
}
```

- **Dataflow, not just the declaration site:** the override follows the
  *variable itself*, by object identity, to every direct assignment to it
  anywhere in the file -- not only the declaration statement's own type and
  initializer. `frameCache["frames"] = 0` above is a plain index
  expression (no new `map[...]` syntax), so it was never the problem; what
  this closes is reassigning the whole value, e.g. `frameCache =
  map[string]int{...}` in a completely different function later in the
  file -- that map literal is now also permitted, because the compiler
  traces it back to `frameCache`'s own declaration-level override rather
  than only checking the file's ambient mode at the literal's own position.
  A *different*, un-overridden variable of the same type is unaffected and
  still errors, even from inside that same function.
  - Scope of the tracking: same-file, direct assignment only, matched by
    resolved object identity (so a local variable that merely shadows the
    overridden name is correctly treated as unrelated). It does not follow
    the value through a function call boundary -- a helper function that
    constructs and *returns* a map, for a caller to assign into the
    overridden variable, is a question about that helper's own file mode
    (§3.1's dialect restriction is about where the type appears lexically,
    not where the resulting value ends up), not something a per-variable
    opt-out can or should reach through. Interprocedural flow was
    considered and deliberately left out: the restriction this directive
    opts out of is lexical (does manual-mode *code* contain a forbidden
    type), and extending that notion across call boundaries would require
    a whole-program alias analysis for a benefit that doesn't match the
    stated use case (a file-local cache).
  - Implementation: `pkgWriter.varGolfMode` (noder/writer.go) maps the
    resolved `*types2.Var` object of every overridden declaration to its
    mode, populated by a pre-pass (`collectVarGolfModes`, mirroring the
    existing `collectFuncGolfModes` pattern from the cross-package work in
    §3.3) for package-level vars so declaration order in the file doesn't
    matter, plus inline as each local (function-scoped) `VarDecl` is
    visited -- a local's own declaration always lexically precedes its
    later uses, so no separate pre-pass is needed for those.
    `declCollector.Visit`'s new `*syntax.AssignStmt` case
    (`assignLhsGolfMode`) resolves the assignment's left-hand side back to
    that same map (by `types2.Info.Uses`, i.e. object identity, not name)
    and, on a match, applies the override to the whole statement's
    subtree exactly the way the original `*syntax.VarDecl` case already
    did -- both return a modified copy of the `declCollector` (its
    `varGolfMode` field) for `syntax.Walk` to use on that subtree, the same
    mechanism `closureScope` already used for closure-capture detection.
    Every manual-mode dialect check (`MapType`, `InterfaceType`, closure
    capture, interface `Name` use) consults a `c.golfMode()` helper
    (override-or-file-mode) instead of reading `c.file.golfMode` directly.
    No change was needed to §3.3's cross-mode boundary/copy checks, which
    key off a *function's* declared mode, not a variable's.
- A modifier naming the mode the declaration is already in (e.g. `manual
  var x` in a file that's already `//golf:mem(manual)`) is a compile error
  ("redundant"), not silently accepted -- same philosophy as rejecting a
  duplicate `//golf:noreflect` method name (§2): a directive that does
  nothing is more likely a mistake than an intentional no-op.
- **Grammar, not a pragma:** `collect`, `manual`, and `real` are soft
  (contextual) keywords, special only in the exact two-token sequence
  "one of these identifiers immediately followed by `var`" -- in
  `cmd/compile/internal/syntax/parser.go`, at both of the places `var`
  starts a declaration (`fileOrNil`'s top-level loop and `stmtOrNil`'s
  local-statement dispatch). Everywhere else they remain perfectly
  ordinary identifiers; this is unambiguous and needs no multi-token
  lookahead buffer, because a bare identifier followed by a newline
  always gets an automatically inserted semicolon before whatever comes
  next (Go's standard rule for identifiers), so `ident` directly
  followed by the literal token `var` with nothing between them on the
  same line could never arise from ordinary Go source in the first
  place -- stock Go already rejects it as a syntax error (an expression
  statement immediately followed by an unexpected `var`). The parser
  checks this by consuming the identifier and peeking at what follows:
  if it's `_Var`, both tokens are consumed as the modifier (recorded on
  a new `syntax.VarDecl.GolfMode` field, no pragma involved); if it's
  anything else, the already-consumed identifier is fed back into
  ordinary expression parsing via two new helpers, `pexpr`/`binaryExpr`
  continuations (`exprListFrom`) that resume exactly where generic
  expression parsing would have been had the identifier never been
  special-cased at all. `go build std cmd` and `go vet std` (both part
  of the usual verification) confirm this: `real`, `manual`, and
  `collect` are all common, completely unremarkable identifiers
  throughout the standard library (variable names, a package-level
  function literally named `collect` would also be fine), and none of
  that broke.
- Verified with the usual full `./make.bash` three-stage bootstrap, `go
  test ./...` across `cmd/compile` (`cmd/compile/internal/syntax`
  included, since that's the package most directly touched), `go build
  std cmd`, `go vet std`, and a stdlib spot-check, plus a hand-written
  positive/negative matrix: a `collect`-overridden var in a manual-mode
  file builds and runs correctly, including reassigning it from a
  different function elsewhere in the file (the dataflow case); an
  un-overridden sibling var of the same (otherwise-forbidden) type in the
  same file still errors, including when assigned to (not just declared);
  `real var` always errors; `manual`/`collect` naming the file's current
  mode errors as redundant; grouped declarations (`collect var ( ... )`)
  and local (function-scoped) declarations both work; a label literally
  named `manual` (no `var` following it) and ordinary identifiers/calls
  named `collect`/`manual`/`real` all still parse and run as plain Go.

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
- The rest of the QOL/general-optimization feature list beyond §5's
  try/catch/success -- not yet enumerated beyond the candidate list in §5.

## 5. QOL features (Go community requests)

Candidate list, roughly ordered by implementation size, of commonly-
requested Go ergonomics improvements that fit GOLF's "keep Go's ergonomics
as the default experience" goal (§1) without turning into a different
language in the common case. Not all of these are committed; each gets
its own scoping pass before implementation, same as everything else in
this document.

1. **Error-propagation sugar** (`try`/`catch`/`success`) -- **done**, see
   below.
2. `??` nil-coalescing for pointers/interfaces.
3. Compile-time `implements` assertion sugar, replacing the
   `var _ I = (*T)(nil)` idiom.
4. Opt-in exhaustive-switch checking over a closed set of consts.
5. Operator overloading for user-defined numeric/vector types, bounded to
   a fixed set of method names (not general operator overloading) --
   particularly relevant given GOLF's game-dev focus.
6. String interpolation, desugaring into `fmt.Sprintf`.
7. Named/default function parameters.
8. `const` aggregate literals (const arrays/structs, not just scalars).
9. First-class sum types / tagged unions -- the single most-requested Go
   *language* feature, but the largest item here by far (new type kind,
   exhaustiveness checking, pattern matching); a separate design pass of
   its own before any code, same as RSR.
10. Lightweight pattern matching / destructuring beyond today's type
    switch.

### 5.1 Error-propagation sugar: `try` / `catch` / `success`

**Status: implemented.** `try`/`catch`/`success` are new contextual
(soft) keywords -- not reserved words, since "try", "catch", and
"success" are all common, completely ordinary Go identifiers (GOLF's own
standard library build confirmed this: `os` has a `success()` method,
several packages use `try` as a local variable or test-helper name, "the
catch" appears in comments). They're special only in the same narrow,
unambiguous sense §3.2's `collect`/`manual`/`real` modifiers are: `try`
is recognized only when immediately followed by `{` with nothing between
them (never arising from legal Go source otherwise -- a bare `try{}`
composite-literal statement naming a type called `try` is the only
theoretical collision, and it doesn't appear anywhere in GOLF's own
stdlib); `catch`/`success` are recognized only immediately after a
`try`-block's closing `}` with no intervening newline, the same
"cuddled brace" requirement stock Go already imposes on `} else {`,
just for soft keywords instead of a reserved one.

```go
try {
    a, err := step1()
    b, err := step2(a)
} catch err {
    log.Println("failed:", err)
} success {
    use(a, b)
}
```

- **Checkpoints:** inside the `try` block, any top-level assignment or
  short variable declaration whose *last* left-hand-side name is
  literally `err` is a checkpoint -- if that statement leaves `err`
  non-nil, control jumps to the `catch` block. This is a naming
  convention, not a type-driven check (matches the established Go idiom
  of naming the error result `err`, and needs no type information at
  parse time, keeping the whole feature a pure syntax-to-syntax
  desugaring -- see below). A statement whose last name isn't `err` is
  left completely alone, and a bare call-expression statement (no
  left-hand side to name) is never auto-checked, same as outside a try
  block -- discarding an error is always an explicit choice. A `try`
  block with zero checkpoints is legal (if unusual): the try and success
  bodies just run unconditionally in sequence, and the compiler omits
  the unreachable catch machinery entirely rather than emit an
  unused-label/unused-variable error.
- **`catch <name> { ... }`** is required, and rebinds the propagated
  error under whatever name the programmer chooses -- it doesn't have to
  match the `err` convention used inside the try block. Known v1
  limitation: the catch block only ever sees the propagated error
  itself, not any partial results from before the failure (the variables
  the try block declared are out of scope there) -- document this,
  rationale below.
- **`success { ... }`** is optional, and runs only if every checkpoint
  passed. Every variable the try block declared *is* in scope here,
  since it's desugared as the literal tail of the same block the try
  statements run in (see below) -- this is the main reason `catch` and
  `success` have asymmetric visibility: extending it to `catch` as well
  would mean the catch label needs to be reachable from a jump that
  skips over those same declarations, which Go's goto-scope rule
  forbids without either variable hoisting (impossible without type
  information -- the whole point of keeping this purely syntactic) or
  duplicating the catch body once per checkpoint (code bloat, and a
  correctness hazard if a cloned subtree were ever accidentally shared
  rather than independently copied). Limiting catch to the error alone
  sidesteps all of that cleanly.
- **`return`, `break`, and `continue` inside `catch` or `success` behave
  exactly as if they'd been written directly in the enclosing function or
  loop** -- `try` is not a closure. This was a deliberate design
  decision, not a side effect: an earlier design considered wrapping the
  whole construct in an immediately-invoked closure (which would have
  avoided some of the scoping work below), but that would have silently
  changed what `return` means inside a catch block, which defeats most
  of the point -- `return err` is by far the most common thing anyone
  will want to write there, replacing exactly the
  `if err != nil { return err }` idiom this feature exists to shorten.
- **Implementation, pure syntax-to-syntax desugaring:** entirely
  contained in `cmd/compile/internal/syntax` (parser.go's `golfTryStmt`,
  wired into `stmtOrNil` the same way `golfVarModOrNil` is) -- no changes
  anywhere else (types2, noder, the backend), since the desugared output
  is built entirely from ordinary `goto`/label/block/var constructs those
  packages already handle for any hand-written Go using them. Given
  ```go
  try { S1; S2 } catch err { C } success { Z }
  ```
  with `S1` a checkpoint and `S2` not, it becomes (synthesized names
  elided for clarity):
  ```go
  var _err error
  {
      S1
      if err != nil {
          _err = err
          goto _catch
      }
      S2
      Z
  }
  goto _done
  _catch:
  {
      err := _err   // rebound under catch's own chosen name
      C
  }
  _done:
  ```
  The hoisted `_err`, declared *before* the inner block (so it's already
  in scope at every `goto` site inside it), is what lets the single
  physical copy of `C` reach every checkpoint without either duplicating
  it or needing a jump to skip into the inner block's own scope -- every
  `goto` here only ever jumps *out* of a block, never into one, which is
  what keeps this legal under Go's "a goto must not cause a variable to
  come into scope that wasn't already in scope at the goto" rule. The
  parser's own `checkBranches` pass (already run automatically at the
  end of every function body) validates the generated `goto`s exactly as
  if a programmer had written them by hand, which caught the two real
  bugs found during development (below) immediately, as ordinary compile
  errors against the generated code -- not something this feature had to
  reimplement.
- **Verified** with a full three-stage `./make.bash` bootstrap, `go test
  ./...` across `cmd/compile` (`syntax` included), `go build`/`vet std
  cmd`, and a hand-written matrix: success-only and catch-triggering
  cases; a zero-checkpoint try block; variables declared in the try body
  visible in `success`; `return`/`continue` from inside `catch` reaching
  the real enclosing function/loop (including from inside a `for` loop,
  confirming the loop's own iteration continues normally); and
  `try`/`catch`/`success` still working as ordinary identifiers,
  function names, and struct field values everywhere else.
- **Bugs found during development:** the DWARF scope-tracking pass
  crashed with "unknown scope position" the first time this ran, because
  the synthesized `*syntax.BlockStmt` nodes never had their `Rbrace`
  position set (only a real `}` token sets it during normal parsing) --
  fixed by giving each one a sensible position. The zero-checkpoint case
  initially always emitted the catch/goto/label scaffolding regardless,
  which correctly failed to compile with "label ... defined and not
  used" once there was truly no checkpoint to reach it from -- fixed by
  detecting that case up front and falling back to a plain inlined block
  with no synthesized control flow at all.
