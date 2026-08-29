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
