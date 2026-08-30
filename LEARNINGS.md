# Interpreter lifecycle learnings

- A context-aware evaluation may return while interpreted code is still blocked
  in a native call. Later evaluations must detach from the canceled execution
  root and serialize compiler/frame preparation without reviving that root.
- Source-package compilation and runtime initialization have different commit
  boundaries. Cache a package only after initialization commits; retain prepared
  compiler output so cancellation or panic can retry initialization on a fresh
  root, and roll back an importing package that never reached preparation.
- Ownership cleanup must not traverse compiler-owned symbol maps while another
  incremental compilation mutates them. Publish immutable host-facing symbol
  and global-slot snapshots at successful compiler boundaries.
- Per-write ownership checks must stay O(1): gate registry walks behind exact
  counters (host-shared objects) and empty registries (channels, panic tokens),
  and compute reachability with one traversal per ownership source instead of
  one containment walk per owned object. Otherwise long-lived interpreters that
  accumulate allocations regress quadratically.
- Propagating host-shared storage with an all-pairs fixpoint re-scans the whole
  registry per pass; a worklist (each object transitions once) has the same
  result at a fraction of the cost.
- A retry after a failed source-package init rewinds the type allocator and can
  reallocate frame indexes with different types. resizeFrameTo must re-align
  drifted cells to the allocator before execution, or stale typed temporaries
  from the canceled attempt panic on first write.
- Interpreted-to-native calls must not allocate a fresh MakeFunc wrapper per
  call; memoize the bound wrapper per (value, root, cancel, signature) on the
  metadata group or alias registration grows the registry per call.
- A canceled evaluation's worker unwinds its deferred calls after the API call
  has returned, so the execution gate no longer excludes it from later
  evaluations. While such a zombie unwinds, its interpreted steps hold the
  funcSweep fence exclusively (tracked by a counter, so nested frames and
  native stretches behave correctly): interpreted deferred writes are fenced
  against a later evaluation's interpreted steps on shared containers. Native
  deferred calls run outside the fence — a host function value deferred by
  interpreted code writes through host code the interpreter cannot fence — and
  a zombie defer blocked in a host call still releases the fence and never
  blocks later evaluations.
- Channel receives store an unaddressable copy. Any interpreter path that
  writes a received value directly into a frame cell must store an addressable
  copy instead, or a later write into that cell (for example the regular-
  return Set for `return <-ch`) panics with "reflect: reflect.Value.Set using
  unaddressable value".
- A step that acquires the funcSweep fence must release it in the mode that was
  acquired, not the mode a re-read of zombieDefers would now derive: the
  counter is flipped by the canceled worker's goroutine and can change mid-step
  between acquisition and release. The acquired mode is captured on the frame
  (with save/restore so a reentrant Eval nesting on the same global frame keeps
  the outer mode); the release helpers consult the capture. Re-deriving the
  mode at release fatal-errors in sync ("RUnlock/Unlock of unlocked RWMutex")
  and kills the embedding process.
- Reentrancy detection for a nested Eval from a host callback must walk the
  entire native stack: a callback under deep native recursion (marshaling,
  comparison, rendering) passes any fixed PC-window, is misread as an unrelated
  goroutine, and deadlocks the nested Eval on the execution gate.
- Known growth boundary: owned allocations are registered per make/new/composite
  literal and released at frame exit or Eval end, so a single frame that runs
  forever (a loop allocating in an interpreted daemon) grows the registry
  without bound (measured: an interpreted loop of map literals reached ~60 GB
  RSS in minutes, while master stays flat). Bounding this safely requires
  mid-execution reachability over all active frame chains, not just the root.
- Known growth boundary: interpreted function values that cross the Eval API
  keep their metadata marked opaque, which is never deleted, so every returned
  or host-retained closure pins its captures and compiled graph permanently
  (measured: ~255 KB per Eval across 20k sequential Evals). PurgeRetainedFuncs
  is the manual reclamation path. The long-term fix — a weak funcval-keyed
  registry that reclaims metadata when the host drops the wrapper — was
  probed and is mechanically feasible but is a full keying rework, not an
  additive layer: as long as funcMeta holds the reflect.Value key, the map
  itself retains the wrapper and no weak mechanism can observe the drop.
  Verified probe results for the future rework: (1) a func value's identity
  is its funcval address, readable as the single word of a func variable and
  identical to reflect.Value's internal comparison identity, so map[uintptr]
  keying preserves today's key semantics; (2) runtime.SetFinalizer accepts
  the funcval address behind a typed pointer and fires exactly when the
  wrapper becomes unreachable — reflect.Value.Pointer() (a code pointer) and
  raw unsafe.Pointer are rejected; (3) address reuse after a drop needs a
  generation guard (finalizer deletes only if the entry's generation still
  matches); (4) every consumer that uses a map key as a reflect.Value
  (types, invocation, endpoint matching in the purge, frame funcMeta
  slices) must switch to the pointer domain, storing reflect.Type in the
  entry for the convertible-type fallback. None of this is additive on top
  of the existing value-keyed map, so it lands only as a dedicated change
  with the isolation and retention suites re-derived.
- Reentrancy for the execution gate is an explicit token scoped to the
  goroutine running the execution, not a native stack probe: any goroutine
  running interpreted code has runCfg on its stack, including goroutines
  spawned by an interpreted `go` statement, so a stack probe treats an Eval
  from such a goroutine as reentrant and lets it run concurrently with the
  gated execution (observed as a data race between prepareExecutionFrame and
  the running execution). The token is set by the execution bodies
  (executeWithPublication and source-package initialization), survives
  arbitrarily deep native callback stacks, nests per interpreter, and is
  never inherited by `go`-statement goroutines, whose Evals wait for the
  gate like any unrelated goroutine.
- Compilation entry points that can import source packages (Compile,
  CompileAST, CompilePath, EvalTest) execute package initialization during
  compilation and must take the execution gate like Eval, or interpreted init
  code overlaps an in-flight evaluation.
- Removing an owned object from the registry is O(1) via its key. Never scan
  the registry to find an object again at deletion time: a per-target scan
  made frame exit quadratic in the allocations of the exiting frame (a
  100k-allocation frame spent ~15s unwinding).
- gc resolves variable-rooted memory operands (selector, index, pointer deref)
  of multi-assignment map destinations at assignment time, like plain
  identifiers. Yaegi still freezes non-ident memory operands before the
  right-hand side, so a right-hand-side call that rebinds the receiver writes
  into the orphaned map; the same late-resolution question applies to slice
  and pointer-deref destinations. A multi-return call RHS into a map-entry
  destination (m[0], x = f()) also still loses the map write.
- Bounding the ownership registries cannot skip registration: allocations made
  before the first cancellation are exactly the ones the first detached-root
  clone must copy, and unregistered native-looking objects are shared with the
  canceled worker (16 detached-root isolation and host-shared tests fail under
  a registration gate). The bound must come from eviction against a root set
  that is a strict superset of every live consumer: durable globals, every
  registered active frame's full ancestor chain, all closure captures, and
  direct funcs. The incremental sweep consumes its trigger at the top of an
  exec step (no locks held) under a TryLock'ed exclusive fence, and stays
  pending while any frame is unwinding defers, because deferred-call values
  are invisible to the root set.
- Purging retained function metadata must be group-scoped: host-bound aliases
  share the metadata group, and lookup's convertible-type fallback would
  resurrect a purged capability through a surviving alias key. Eligibility
  (pending refcounts, panic tokens, live channel sends) must be re-checked
  under the delete-phase lock: panic-token memberships attach under funcMu
  without bumping the group version.
- A func-bearing-value walk can be inverted from visitor-per-registry-entry
  into collect-once-then-intersect (exact canonical set + an any-ambiguous
  flag) with identical results, because pointer/slice/map containers can only
  ever yield ambiguous matches; this removed the per-Eval quadratic term in
  accumulated opaque metadata.
- The directFuncs activation cache must be swept like the ownership
  registries: each entry pins its root frame's globals and its cloned
  wrapper, so a long-lived interpreter otherwise grows one pinned root per
  detached generation plus one dead line per swept-away source. The
  incremental ownedGC sweep evicts entries whose root is no longer live
  (durable root, active frame chain, or retained metadata) or whose source
  and value endpoints both lost their metadata, mirroring the metadata
  purge's endpoint rule. Eviction is transparent: the cache is consulted
  only after a successful metadata lookup keyed by the live activation
  root, so a dropped line costs at most one extra clone.
- Known open item (decision recorded 2026-08-30): an incremental Eval
  beginning with `func` permanently adds 1-3 global-frame slots on this
  branch (master adds none) because the anti-replay fix returns the wrapper
  body, which compiles in global scope where each func literal allocates a
  persistent slot (cfg.go funcLit pushes n.findex = sc.add(n.typ) in the
  enclosing scope). Wrapping the body in a one-shot function literal does
  not help: the literal's own value slot is allocated in the enclosing
  scope before its function scope is pushed. The chosen future direction is
  codegen for immediately-called function literals at root level (a
  callExpr whose fun is a funcLit compiles to a direct activation without a
  registry slot); transient slot recycling was rejected because root-frame
  slots are reachable by goroutine-safe code paths (a `go` statement can
  capture any slot), and the scheduler-level one-shot declaration remains
  rejected. Severity is REPL-shaped (thousands of func-Evals on one
  interpreter); behavior is otherwise master-compatible.
