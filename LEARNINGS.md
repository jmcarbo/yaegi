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
  native stretches behave correctly): deferred writes can never race a later
  evaluation's writes on shared containers, and a zombie defer blocked in a
  host call still releases the fence and never blocks later evaluations.
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
  (measured: ~255 KB per Eval across 20k sequential Evals). A host-facing
  purge API or self-describing wrapper metadata is needed before long-lived
  REPL-style interpreters are safe.
- The context-aware gate path (EvalWithContext, EvalPathWithContext,
  ExecuteWithContext) needs the same reentrancy bypass as the plain gate: a
  host callback calling back with a context otherwise waits for the gate its
  own outer evaluation holds, until the context expires (forever with
  context.Background).
- Reentrancy detection by stack scan has a known false positive: any goroutine
  running interpreted code has runCfg on its stack, including goroutines
  spawned by an interpreted `go` statement, so an Eval from a host callback on
  such a goroutine bypasses the gate and can run concurrently with the gated
  execution (observed as a data race between prepareExecutionFrame and the
  running execution). The sound fix is an explicit execution/reentrancy token
  threaded across the host-call boundary, not a stack probe.
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
