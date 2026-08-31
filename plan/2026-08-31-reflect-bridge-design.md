# Host bridge design: native boxes for interpreted method sets (FINAL)

Status: implemented on branch jmca/reflect-bridge. See LEARNINGS.md for the
loaded-bearing details. This document records the architecture as built.

## Core mechanism

yaegi represents interpreted values crossing into host code as either:

1. `interp.valueInterface{node, value}` — the interpreted dynamic type
   carrier (frame cells of interpreted non-empty interfaces materialize as
   this). Opaque to host code.
2. Raw concrete values — the "concrete unwrap" policy for binary
   `interface{}` params (`genValueConcrete`/`unwrapInterfaceValue`): right
   shape for reflection over data, but the method set is invisible.
3. Interface wrapper boxes — struct with an `IValue` first field plus one
   `W<Method> func` field per method, with real promoted methods so the box
   genuinely implements the native interface (`_error`, `_fmt_Stringer`,
   `_encoding_json_Marshaler`, ...). Boxes are the substitute for method
   synthesis: Go reflect cannot create named types with methods at runtime.

The bridge layer (interp/bridge.go) generalizes (3) with:

- **Catalog** (bridgeState/rebuildBridgeCatalog): indexes every registered
  box type (binPkg `_`-prefixed symbols + composed mapTypes wrappers), most
  specific first, ties broken by type name for determinism. Answers "which
  box can carry this interpreted method set".
- **Reverse registry** (rtoitype): named interpreted struct rtypes map back
  to their itype (T and *T), so a raw concrete value that came back from
  binary land can be traced to its interpreted method set. Feeds the
  reflect.Value method bridge and the API-boundary re-boxing.
- **Host bridges** (RegisterBridge[T]): host-supplied adapter factories for
  host-declared interfaces, keyed by method-set match; the only way to
  satisfy an arbitrary host interface (no method synthesis exists).

## Crossing policies (per function, never a blanket catalog at unwrap sites)

- **Variadic non-empty interface elements**: callBin wraps each argument of
  `...error`-style slots against the element interface (fixes
  errors.Join/Is panics).
- **Wrap policy** (fmt.Errorf only): direct concrete values implementing
  `Error() string` box into `_error` so `%w` produces a real wrap chain.
  Boxing for the whole print family would break non-error verbs (`%d` on a
  named int with an Error method sees an opaque box) — rejected.
- **Out-parameter bridge** (errors.As): pointer-to-pointer arguments whose
  pointee implements error cross as `*_errorCell{_error; Want}`; `_error.As`
  matches targets by the carried type identity; callBin runs a write-back
  (inside a fence-released stretch) moving the found error into the
  interpreted target cell. Unmatched calls write the seeded value back
  (no-op).
- **Deep bridge, read-only** (json.Marshal/MarshalIndent/Encoder.Encode,
  xml.Marshal family — the Encode entries were missing from mapTypes):
  containers are rebuilt with bridged elements so encoding/json dispatches
  interpreted MarshalJSON per element, including addressable elements with
  pointer-receiver methods (json's addrMarshalerEncoder semantics).
- **Deep bridge, inout** (json.Unmarshal, xml.Unmarshal): target containers
  are mirrored with Unmarshaler-satisfying leaves widened to interface{}
  (native leaves stay typed so json fills them structurally); the write-back
  re-marshals each decoded generic leaf and feeds it through the interpreted
  UnmarshalJSON writing the original element cell. Widening (not pre-seeded
  boxes) is what makes json-allocated elements work.
- **Pointer alias** (Unmarshaler targets): a pointer whose pointee satisfies
  the (substituted) parameter interface passes a pointer to a box bound
  through the pointer, so json.Unmarshal dispatches writes straight into the
  interpreted value.
- **Eval API boundary** (executeWithPublication, before the ownership
  sweeps): valueInterface-typed cells and any-cells carrying interpreted
  concretes are re-boxed (host bridge, then catalog box, else raw concrete).
  The same conversion runs on interpreted results crossing to host through
  function wrappers (invokeInterpretedHostBoundary). valueInterface never
  leaks to host code.
- **Unbox on re-entry**: typeAssert, type switches and getMethodByName
  recover the interpreted view of bridge boxes (IValue carries a
  valueInterface in bridge-built boxes, the concrete in legacy boxes) and of
  raw concretes (reverse registry). Method-less values keep the unwrap
  semantics (structural reflection over plain data is preserved).
- **reflect.Value method bridge** (#847): the Method/MethodByName/NumMethod
  family on a reflect.Value whose content maps back to an interpreted type
  returns MakeFunc'd method values bound to the interpreted receiver
  (receiver node left nil so the carried value is used directly). Native
  method-set rules mirrored; Type-level introspection deliberately not
  bridged (unsound to fake reflect.Type) — documented asymmetry.
- **#1534 guard**: refType rejects value cycles through named struct fields
  (invalid recursive types the compiler rejects) with a proper compilation
  error, recovered at the cfg boundary; legal pointer/slice/map/func
  recursion untouched. The recursive-rebuild patch loop now patches each
  field from its own declared itype (the shared ctx.rect silently corrupted
  field types when several ancestors were involved).

## Status per issue

- #847: fixed (interpreted-side reflect method introspection).
- #939: fixed with RegisterBridge; without registration the host sees the
  raw concrete value (documented limit).
- #681: fixed (reflect round trip through interpreted assertions).
- #1345: fixed (both the nil case and As-through-%w with target write-back).
- #1486: fixed (per-element marshal and unmarshal dispatch through
  containers).
- #1490: was passing accidentally (exported-name mangling made the round
  trip structural); pinned as a canary, passes through real dispatch.
- #1534: fixed at the root (invalid recursive types rejected; mis-patch
  fixed).
- go-jose: interface satisfaction fixed; type identity/names remain
  impossible (anonymous StructOf types) — documented hard boundary.

## Non-goals (documented in LEARNINGS.md)

- Runtime synthesis of named native types with methods (impossible in Go).
- Host-side reflect.MethodByName on interpreted receivers.
- Deep bridging outside the per-function policy set (mutation contracts:
  sort.Slice-style in-place mutation must keep working).
