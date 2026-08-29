package interp

import (
	"reflect"
	"runtime"
)

type funcFrameState uint8

type funcMetaRetention uint8

type funcMetaGroup struct {
	_           byte
	pending     int
	received    bool
	root        *frame
	version     uint64
	captures    []funcMetaCapture
	panicTokens map[*ownedPanicToken]struct{}
	// bound memoizes host-bound MakeFunc wrappers per (target, activation,
	// signature, boundary mode) so repeated native calls carrying the same
	// interpreted argument reuse one wrapper instead of allocating and
	// registering a fresh alias per call. Guarded by funcMu.
	bound map[boundWrapperKey]reflect.Value
}

type funcMetaCapture struct {
	frame *frame
	index int
}

type funcMetaCaptureRef struct {
	level int
	index int
}

// interpretedFuncCaptureRefs records the exact lexical slots a function
// literal (including nested literals which close over the same outer scope)
// reads from its creation environment. Package globals are resolved through
// the active root and therefore are not lexical captures.
func interpretedFuncCaptureRefs(n *node) []funcMetaCaptureRef {
	if n == nil || len(n.child) < 4 {
		return nil
	}
	seen := map[funcMetaCaptureRef]struct{}{}
	refs := []funcMetaCaptureRef{}
	var walk func(*node, int)
	walk = func(current *node, depth int) {
		if current == nil {
			return
		}
		if current != n && current.kind == funcLit {
			depth++
		}
		if current.kind == identExpr && current.findex >= 0 && current.level != globalFrame && current.level > depth {
			ref := funcMetaCaptureRef{level: current.level - depth - 1, index: current.findex}
			if _, ok := seen[ref]; !ok {
				seen[ref] = struct{}{}
				refs = append(refs, ref)
			}
		}
		for _, child := range current.child {
			walk(child, depth)
		}
	}
	walk(n.child[3], 0)
	return refs
}

func resolveInterpretedFuncCaptures(captured *frame, refs []funcMetaCaptureRef) []funcMetaCapture {
	if captured == nil || len(refs) == 0 {
		return nil
	}
	result := make([]funcMetaCapture, 0, len(refs))
	for _, ref := range refs {
		owner := getFrame(captured, ref.level)
		if owner == nil || ref.index < 0 || ref.index >= len(owner.data) {
			continue
		}
		result = append(result, funcMetaCapture{frame: owner, index: ref.index})
	}
	return result
}

const (
	funcFrameInactive funcFrameState = iota
	funcFrameActive
	funcFrameReleasing
	funcFrameFinished
)

const (
	funcMetaVisible funcMetaRetention = iota
	funcMetaChannel
	funcMetaPanic
	funcMetaOpaque
)

// withFuncSweepWriteFromExec upgrades the execution fence while escape
// metadata changes, then restores the read fence used by the CFG runner.
func (interp *Interpreter) withFuncSweepWriteFromExec(run func()) {
	interp.funcSweepMu.RUnlock()
	interp.funcSweepMu.Lock()
	defer func() {
		interp.funcSweepMu.Unlock()
		interp.funcSweepMu.RLock()
	}()
	run()
}

func (interp *Interpreter) beginInterpretedFuncPanic(value reflect.Value) *interpretedPanic {
	frozen := value
	if !unwrapOwnedValue(value).IsValid() {
		frozen = reflect.ValueOf(&runtime.PanicNilError{})
	} else {
		frozen = reflect.New(value.Type()).Elem()
		frozen.Set(value)
	}
	state := &interpretedPanic{value: frozen}
	interp.withFuncSweepWriteFromExec(func() {
		state.token = interp.beginOwnedPanicLocked(frozen)
	})
	return state
}

func (interp *Interpreter) markInterpretedFuncChannelSend(f *frame, channel, value reflect.Value) *ownedChannelSend {
	var send *ownedChannelSend
	interp.withFuncSweepWriteFromExec(func() {
		interp.funcMu.RLock()
		owned := interp.ownedChannelLocked(channel)
		hostVisible := owned == nil || owned.hostVisible
		interp.funcMu.RUnlock()
		if hostVisible {
			interp.markOwnedValuesHostSharedLocked(value)
			interp.preserveReturnedInterpretedFuncsLocked(value)
			return
		}
		interp.funcMu.Lock()
		send = interp.recordOwnedChannelSendLocked(channel, value, f)
		interp.funcMu.Unlock()
	})
	return send
}

func (interp *Interpreter) commitInterpretedFuncChannelSend(send *ownedChannelSend) {
	if send == nil {
		return
	}
	interp.withFuncSweepWriteFromExec(func() {
		interp.funcMu.Lock()
		if send.state == ownedChannelSendPrepared {
			send.state = ownedChannelSendDelivered
		}
		interp.funcMu.Unlock()
	})
}

func (interp *Interpreter) rollbackInterpretedFuncChannelSend(send *ownedChannelSend) {
	if send == nil {
		return
	}
	interp.withFuncSweepWriteFromExec(func() {
		interp.funcMu.Lock()
		interp.retireOwnedChannelSendLocked(send)
		interp.releaseOwnedChannelSendFuncsLocked(send, send.funcs, nil, false)
		interp.funcMu.Unlock()
	})
}

func (interp *Interpreter) rollbackInterpretedFuncPanicEscape(recovered interface{}) {
	interp.funcSweepMu.Lock()
	defer interp.funcSweepMu.Unlock()
	value, token := splitInterpretedPanic(recovered)
	interp.finishOwnedPanicToken(token)
	if token != nil {
		return
	}
	reflected := reflect.ValueOf(value)
	interp.funcMu.RLock()
	entries := make(map[reflect.Value]interpretedFuncMeta, len(interp.funcMeta))
	for key, meta := range interp.funcMeta {
		entries[key] = meta
	}
	interp.funcMu.RUnlock()
	groups := map[*funcMetaGroup]struct{}{}
	for key, meta := range entries {
		visitor := funcValueVisitor{targets: map[reflect.Value]struct{}{key: {}}}
		if visitor.contains(reflected) {
			groups[meta.group] = struct{}{}
		}
	}
	interp.funcMu.Lock()
	defer interp.funcMu.Unlock()
	for key, meta := range interp.funcMeta {
		if _, ok := groups[meta.group]; !ok {
			continue
		}
		if meta.retention == funcMetaPanic {
			meta.retention = funcMetaVisible
			interp.funcMeta[key] = meta
		}
		if meta.frame != nil && meta.frame != meta.frame.root && meta.frame.funcEscape == funcMetaPanic {
			meta.frame.funcEscape = funcMetaVisible
		}
	}
}

func (interp *Interpreter) markInterpretedFuncMetadataEscapedLocked(retention funcMetaRetention, values ...reflect.Value) {
	interp.funcMu.RLock()
	entries := make(map[reflect.Value]interpretedFuncMeta, len(interp.funcMeta))
	for key, meta := range interp.funcMeta {
		entries[key] = meta
	}
	interp.funcMu.RUnlock()

	escaped := map[reflect.Value]struct{}{}
	for key := range entries {
		visitor := funcValueVisitor{targets: map[reflect.Value]struct{}{key: {}}}
		for _, value := range values {
			if visitor.contains(value) {
				escaped[key] = struct{}{}
				break
			}
		}
	}
	if len(escaped) == 0 {
		return
	}

	interp.funcMu.Lock()
	rootGroups := map[*funcMetaGroup]struct{}{}
	markedGroups := map[*funcMetaGroup]struct{}{}
	for key := range escaped {
		meta, ok := interp.funcMeta[key]
		if !ok || meta.frame == nil {
			continue
		}
		if meta.group != nil {
			markedGroups[meta.group] = struct{}{}
		}
		if meta.frame == meta.frame.root {
			rootGroups[meta.group] = struct{}{}
		} else if retention > meta.frame.funcEscape {
			meta.frame.funcEscape = retention
		}
	}
	for key, meta := range interp.funcMeta {
		if _, ok := rootGroups[meta.group]; ok && retention > meta.retention {
			meta.retention = retention
			interp.funcMeta[key] = meta
		}
	}
	if retention == funcMetaChannel {
		for group := range markedGroups {
			group.pending++
		}
	}
	interp.funcMu.Unlock()
}

// releaseInterpretedFuncs removes metadata for wrappers whose creating frame
// has finished and whose values cannot escape that frame. Root registrations
// are permanent. Any ambiguous escape retains the complete frame group, since
// one reachable closure can itself capture another wrapper opaquely.
func (interp *Interpreter) releaseInterpretedFuncs(f *frame, funcNode *node, recovered interface{}) {
	if f == nil || f == f.root {
		return
	}
	interp.funcSweepMu.Lock()
	defer interp.funcSweepMu.Unlock()
	if recovered != nil {
		var token *ownedPanicToken
		recovered, token = splitInterpretedPanic(recovered)
		if token == nil {
			interp.markInterpretedFuncMetadataEscapedLocked(funcMetaPanic, reflect.ValueOf(recovered))
		}
	}

	interp.funcMu.Lock()
	f.funcState = funcFrameReleasing
	keys := append([]reflect.Value(nil), f.funcMeta...)
	escape := f.funcEscape
	interp.funcMu.Unlock()
	if len(keys) == 0 {
		interp.finishInterpretedFuncs(f, nil, false, funcMetaVisible)
		return
	}

	targets := make(map[reflect.Value]struct{}, len(keys))
	for _, key := range keys {
		targets[key] = struct{}{}
	}
	if escape != funcMetaVisible {
		interp.finishInterpretedFuncs(f, f.root, true, escape)
		return
	}
	if frameReturnsFunction(f, funcNode, targets) {
		interp.finishInterpretedFuncs(f, f.anc, true, funcMetaVisible)
		return
	}
	if owner := funcsReachableFromAncestors(f, targets); owner != nil {
		interp.finishInterpretedFuncs(f, owner, true, funcMetaVisible)
		return
	}
	interp.finishInterpretedFuncs(f, nil, false, funcMetaVisible)
}

// finishInterpretedFuncs atomically closes a frame to child transfers and
// either deletes its group or moves it to a live ancestor. A child which
// finishes concurrently after this frame entered the releasing state is
// redirected to the root, so no registration can be appended after the sweep
// snapshot and then lose its next sweep owner.
func (interp *Interpreter) finishInterpretedFuncs(from, to *frame, retain bool, retention funcMetaRetention) {
	interp.funcMu.Lock()
	defer interp.funcMu.Unlock()
	if from.funcEscape != funcMetaVisible {
		retain = true
		to = from.root
		retention = from.funcEscape
	}
	if retain {
		to = funcRetentionOwnerLocked(from.root, to)
	}
	group := &funcMetaGroup{root: from.root, version: 1}
	seenGroups := map[*funcMetaGroup]struct{}{}
	for _, key := range from.funcMeta {
		if meta, ok := interp.funcMeta[key]; ok && meta.frame == from && meta.group != nil {
			seenGroups[meta.group] = struct{}{}
		}
	}
	seenCaptures := map[funcMetaCapture]struct{}{}
	for oldGroup := range seenGroups {
		for _, capture := range oldGroup.captures {
			if _, ok := seenCaptures[capture]; ok {
				continue
			}
			seenCaptures[capture] = struct{}{}
			group.captures = append(group.captures, capture)
		}
		if retain && to == from.root {
			for token := range oldGroup.panicTokens {
				if group.panicTokens == nil {
					group.panicTokens = map[*ownedPanicToken]struct{}{}
				}
				group.panicTokens[token] = struct{}{}
				if _, raw := token.groups[oldGroup]; raw {
					token.groups[group] = struct{}{}
				}
				if _, pending := token.pendingGroups[oldGroup]; pending {
					token.pendingGroups[group] = struct{}{}
				}
			}
		}
	}
	for _, key := range from.funcMeta {
		meta, ok := interp.funcMeta[key]
		if !ok || meta.frame != from {
			continue
		}
		if retain {
			meta.frame = to
			meta.retention = retention
			if to == from.root {
				meta.group = group
			}
			interp.funcMeta[key] = meta
			to.funcMeta = append(to.funcMeta, key)
		} else {
			delete(interp.funcMeta, key)
		}
	}
	from.funcMeta = nil
	from.funcState = funcFrameFinished
	if retention == funcMetaChannel {
		for _, channel := range interp.ownedChannels {
			for _, send := range channel.sends {
				if send.state != ownedChannelSendTerminal {
					interp.refreshOwnedChannelSendLocked(send)
				}
			}
		}
	}
}

func funcRetentionOwnerLocked(root, owner *frame) *frame {
	if owner == nil {
		return root
	}
	if owner.cloneOf != nil {
		owner = owner.cloneOf
	}
	if owner != root && owner.funcState != funcFrameActive {
		return root
	}
	return owner
}

// adoptInterpretedFuncValues moves a delivered escape group into the receiving
// activation. Pending channel values stay protected while queued, but after a
// receive they are ordinary interpreter-visible values and can be reclaimed
// when the receiver and its globals no longer reference them.
func (interp *Interpreter) adoptInterpretedFuncValues(f *frame, source funcMetaRetention, channel reflect.Value, values ...reflect.Value) []reflect.Value {
	adopted := values
	interp.withFuncSweepWriteFromExec(func() {
		if source == funcMetaChannel {
			interp.funcMu.Lock()
			send := interp.consumeOwnedChannelSendLocked(channel, values...)
			interp.funcMu.Unlock()
			if send != nil {
				if send.pendingRoot == f.root && send.pending.IsValid() && len(values) == 1 {
					adopted = []reflect.Value{send.pending}
				} else {
					adopted = values
				}
				adopted = interp.adoptOwnedValuesLocked(f, funcMetaVisible, adopted...)
				interp.funcMu.Lock()
				interp.retireOwnedChannelSendLocked(send)
				interp.releaseOwnedChannelSendFuncsLocked(send, send.funcs, f.root, send.pending.IsValid())
				interp.releaseOwnedChannelSendFuncsLocked(send, send.pendingFuncs, f.root, false)
				interp.funcMu.Unlock()
				return
			}
		}
		adopted = interp.adoptInterpretedFuncValuesLocked(f, source, values...)
	})
	return adopted
}

func (interp *Interpreter) releaseOwnedChannelSendFuncsLocked(send *ownedChannelSend, funcs map[reflect.Value]struct{}, receivingRoot *frame, replaced bool) {
	for key := range funcs {
		usedElsewhere := false
		for _, channel := range interp.ownedChannels {
			for _, other := range channel.sends {
				if other == send || other.state == ownedChannelSendTerminal {
					continue
				}
				if _, ok := other.funcs[key]; ok {
					usedElsewhere = true
				}
				if _, ok := other.pendingFuncs[key]; ok {
					usedElsewhere = true
				}
			}
		}
		if usedElsewhere {
			continue
		}
		meta, ok := interp.funcMeta[key]
		if !ok || meta.retention == funcMetaOpaque {
			continue
		}
		if replaced {
			delete(interp.funcMeta, key)
			continue
		}
		root := receivingRoot
		if root == nil && meta.frame != nil {
			root = meta.frame.root
		}
		if root == nil {
			continue
		}
		meta.frame = root
		meta.retention = funcMetaVisible
		interp.funcMeta[key] = meta
		root.funcMeta = append(root.funcMeta, key)
	}
}

func (interp *Interpreter) adoptInterpretedFuncValue(f *frame, channel, value reflect.Value) reflect.Value {
	values := interp.adoptInterpretedFuncValues(f, funcMetaChannel, channel, value)
	if len(values) == 0 {
		return value
	}
	return values[0]
}

func (interp *Interpreter) adoptInterpretedFuncPanicValue(f *frame, token *ownedPanicToken, value reflect.Value) reflect.Value {
	values := []reflect.Value{value}
	interp.withFuncSweepWriteFromExec(func() {
		if token == nil {
			values = interp.adoptInterpretedFuncValuesLocked(f, funcMetaPanic, values...)
		} else {
			values = interp.adoptOwnedPanicValuesLocked(f, token, values...)
		}
	})
	return values[0]
}

func (interp *Interpreter) adoptInterpretedFuncValuesLocked(f *frame, source funcMetaRetention, values ...reflect.Value) []reflect.Value {
	values = interp.adoptOwnedValuesLocked(f, source, values...)
	return interp.adoptInterpretedFuncMetadataValuesLocked(f, source, values...)
}

func (interp *Interpreter) adoptInterpretedFuncMetadataValuesLocked(f *frame, source funcMetaRetention, values ...reflect.Value) []reflect.Value {
	interp.funcMu.RLock()
	entries := make(map[reflect.Value]interpretedFuncMeta, len(interp.funcMeta))
	for key, meta := range interp.funcMeta {
		eligible := meta.retention == source
		if meta.frame != nil && meta.frame != meta.frame.root {
			eligible = meta.frame.funcEscape == source
		}
		if eligible {
			entries[key] = meta
		}
	}
	interp.funcMu.RUnlock()

	groups := map[*funcMetaGroup]struct{}{}
	for key, meta := range entries {
		if meta.group == nil {
			continue
		}
		visitor := funcValueVisitor{targets: map[reflect.Value]struct{}{key: {}}}
		for _, value := range values {
			if visitor.contains(value) {
				groups[meta.group] = struct{}{}
				break
			}
		}
	}
	if len(groups) == 0 {
		return values
	}

	interp.funcMu.Lock()
	defer interp.funcMu.Unlock()
	if source == funcMetaChannel {
		for group := range groups {
			if group == nil || group.pending == 0 {
				delete(groups, group)
				continue
			}
			group.pending--
			group.received = true
			group.root = f.root
			if group.pending != 0 {
				delete(groups, group)
				continue
			}
			interp.makeChannelGroupVisibleLocked(group, f.root)
			delete(groups, group)
		}
	}
	seenOwner := map[*frame]struct{}{}
	for key, meta := range interp.funcMeta {
		if _, ok := groups[meta.group]; !ok {
			continue
		}
		old := meta.frame
		if old == nil || (meta.retention != source && (old == old.root || old.funcEscape != source)) {
			continue
		}
		meta.frame = f
		meta.retention = funcMetaVisible
		interp.funcMeta[key] = meta
		f.funcMeta = append(f.funcMeta, key)
		if old != nil && old != old.root {
			seenOwner[old] = struct{}{}
		}
	}
	for owner := range seenOwner {
		owner.funcEscape = funcMetaVisible
	}
	return values
}

func (interp *Interpreter) makeChannelGroupVisibleLocked(group *funcMetaGroup, receivingRoot *frame) {
	for key, meta := range interp.funcMeta {
		if meta.group != group {
			continue
		}
		old := meta.frame
		if old == nil {
			continue
		}
		root := receivingRoot
		if root == nil {
			root = old.root
		}
		meta.frame = root
		meta.retention = funcMetaVisible
		interp.funcMeta[key] = meta
		root.funcMeta = append(root.funcMeta, key)
		if old != root && old.funcEscape == funcMetaChannel {
			old.funcEscape = funcMetaVisible
		}
	}
}

// preserveReturnedInterpretedFuncs protects groups which cross the Eval API.
// The host may retain and later return these values after a canceled root was
// detached, so they cannot participate in interpreter-visible root sweeping.
func (interp *Interpreter) preserveReturnedInterpretedFuncs(value reflect.Value) {
	interp.funcSweepMu.Lock()
	defer interp.funcSweepMu.Unlock()
	interp.preserveReturnedInterpretedFuncsLocked(value)
}

func (interp *Interpreter) preserveReturnedInterpretedFuncsLocked(value reflect.Value) {
	if !value.IsValid() {
		return
	}
	interp.funcMu.RLock()
	entries := make(map[reflect.Value]interpretedFuncMeta, len(interp.funcMeta))
	for key, meta := range interp.funcMeta {
		entries[key] = meta
	}
	interp.funcMu.RUnlock()

	groups := map[*funcMetaGroup]struct{}{}
	for key, meta := range entries {
		visitor := funcValueVisitor{targets: map[reflect.Value]struct{}{key: {}}}
		if visitor.contains(value) {
			groups[meta.group] = struct{}{}
		}
	}
	if len(groups) == 0 {
		return
	}
	interp.funcMu.Lock()
	for key, meta := range interp.funcMeta {
		if _, ok := groups[meta.group]; ok {
			meta.retention = funcMetaOpaque
			interp.funcMeta[key] = meta
		}
	}
	interp.funcMu.Unlock()
}

// refreshGlobalVarIndexesLocked publishes stable symbol and variable-slot
// views. The caller holds compileMu, which serializes every mutation of the
// compiler-owned source-package symbol maps.
func (interp *Interpreter) refreshGlobalVarIndexesLocked() {
	indexes := map[int]struct{}{}
	published := imports{}
	interp.mutex.RLock()
	for path, pkg := range interp.srcPkg {
		symbols := map[string]*symbol{}
		for name, sym := range pkg {
			if sym != nil {
				copy := *sym
				symbols[name] = &copy
			}
			if sym != nil && sym.global && sym.kind == varSym && sym.index >= 0 {
				indexes[sym.index] = struct{}{}
			}
		}
		published[path] = symbols
	}
	interp.mutex.RUnlock()
	interp.mutex.Lock()
	interp.globalVarIndexes = indexes
	interp.publishedSrcPkg = published
	interp.mutex.Unlock()
}

func (interp *Interpreter) snapshotGlobalVarIndexes() map[int]struct{} {
	interp.mutex.RLock()
	indexes := make(map[int]struct{}, len(interp.globalVarIndexes))
	for index := range interp.globalVarIndexes {
		indexes[index] = struct{}{}
	}
	interp.mutex.RUnlock()
	return indexes
}

// sweepRootInterpretedFuncs removes only interpreter-visible root metadata.
// Opaque API returns and channel groups which have not yet been received are
// intentionally excluded. Package-global symbol slots are the durable roots;
// temporary CFG slots must not keep consumed callbacks alive indefinitely.
func (interp *Interpreter) sweepRootInterpretedFuncs(root *frame, result reflect.Value) {
	if root == nil {
		return
	}
	indexes := interp.snapshotGlobalVarIndexes()

	locked := false
	for attempt := 0; attempt < 64; attempt++ {
		if interp.funcSweepMu.TryLock() {
			locked = true
			break
		}
		runtime.Gosched()
	}
	if !locked {
		// The exclusive fence stayed contended (a worker is executing or
		// unwinding in native code). Skip this sweep round instead of
		// demoting every visible wrapper to opaque: opaque retention has no
		// demotion path, so escalating here would permanently pin every
		// wrapper and its frames. The next uncontended Eval end sweeps.
		return
	}
	defer interp.funcSweepMu.Unlock()
	interp.preserveReturnedInterpretedFuncsLocked(result)

	interp.funcMu.RLock()
	candidates := map[reflect.Value]interpretedFuncMeta{}
	groupCaptures := map[*funcMetaGroup][]funcMetaCapture{}
	groupVersions := map[*funcMetaGroup]uint64{}
	directValues := []reflect.Value{}
	for key, meta := range interp.funcMeta {
		if meta.frame == root && meta.retention == funcMetaVisible {
			candidates[key] = meta
			if meta.group != nil {
				if _, ok := groupVersions[meta.group]; !ok {
					groupCaptures[meta.group] = append([]funcMetaCapture(nil), meta.group.captures...)
					groupVersions[meta.group] = meta.group.version
				}
			}
		}
	}
	for key, value := range interp.directFuncs {
		if key.root == root {
			directValues = append(directValues, value)
		}
	}
	interp.funcMu.RUnlock()
	if len(candidates) == 0 {
		return
	}

	root.mutex.RLock()
	values := make([]reflect.Value, 0, len(indexes))
	for index := range indexes {
		if index < len(root.data) {
			values = append(values, root.data[index])
		}
	}
	root.mutex.RUnlock()
	values = append(values, directValues...)

	liveGroups := map[*funcMetaGroup]struct{}{}
	for key, meta := range candidates {
		visitor := funcValueVisitor{targets: map[reflect.Value]struct{}{key: {}}}
		for _, value := range values {
			if visitor.possiblyContains(value) {
				liveGroups[meta.group] = struct{}{}
				break
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for group := range liveGroups {
			for _, capture := range groupCaptures[group] {
				value, ok := snapshotFuncMetaCapture(capture)
				if !ok {
					continue
				}
				for key, meta := range candidates {
					if _, live := liveGroups[meta.group]; live {
						continue
					}
					visitor := funcValueVisitor{targets: map[reflect.Value]struct{}{key: {}}}
					if visitor.possiblyContains(value) {
						liveGroups[meta.group] = struct{}{}
						changed = true
					}
				}
			}
		}
	}

	interp.deleteUnreachableRootFuncMeta(root, candidates, liveGroups, groupVersions)
}

func snapshotFuncMetaCapture(capture funcMetaCapture) (reflect.Value, bool) {
	if capture.frame == nil {
		return reflect.Value{}, false
	}
	if capture.frame.interp != nil {
		capture.frame.interp.funcMu.RLock()
		defer capture.frame.interp.funcMu.RUnlock()
	}
	capture.frame.mutex.RLock()
	defer capture.frame.mutex.RUnlock()
	if capture.index < 0 || capture.index >= len(capture.frame.data) {
		return reflect.Value{}, false
	}
	value := capture.frame.data[capture.index]
	if !value.IsValid() {
		return reflect.Value{}, false
	}
	if capture.frame.interp != nil && capture.frame.interp.ownedCellHostSharedLocked(value) {
		return reflect.Value{}, false
	}
	snapshot := reflect.New(value.Type()).Elem()
	snapshot.Set(value)
	return snapshot, true
}

func (interp *Interpreter) deleteUnreachableRootFuncMeta(root *frame, candidates map[reflect.Value]interpretedFuncMeta, liveGroups map[*funcMetaGroup]struct{}, groupVersions map[*funcMetaGroup]uint64) {
	interp.funcMu.Lock()
	for key, candidate := range candidates {
		meta, ok := interp.funcMeta[key]
		if !ok || meta.frame != root || meta.retention != funcMetaVisible || meta.group != candidate.group {
			continue
		}
		if meta.group != nil && meta.group.version != groupVersions[meta.group] {
			continue
		}
		if _, live := liveGroups[meta.group]; !live {
			delete(interp.funcMeta, key)
		}
	}
	root.funcMeta = root.funcMeta[:0]
	for key, meta := range interp.funcMeta {
		if meta.frame == root {
			root.funcMeta = append(root.funcMeta, key)
		}
	}
	interp.funcMu.Unlock()
}

func frameReturnsFunction(f *frame, funcNode *node, targets map[reflect.Value]struct{}) bool {
	if funcNode == nil || funcNode.typ == nil {
		return false
	}
	numRet := len(funcNode.typ.ret)
	if numRet > len(f.data) {
		numRet = len(f.data)
	}
	for _, value := range f.data[:numRet] {
		visitor := funcValueVisitor{targets: targets}
		if visitor.contains(value) {
			return true
		}
	}
	return false
}

func funcsReachableFromAncestors(f *frame, targets map[reflect.Value]struct{}) *frame {
	seenFrames := map[*frame]struct{}{}
	for ancestor := f.anc; ancestor != nil; ancestor = ancestor.anc {
		if funcsReachableFromFrame(ancestor, targets, seenFrames) {
			return ancestor
		}
	}
	if funcsReachableFromFrame(f.root, targets, seenFrames) {
		return f.root
	}
	return nil
}

func funcsReachableFromFrame(f *frame, targets map[reflect.Value]struct{}, seenFrames map[*frame]struct{}) bool {
	if f == nil {
		return false
	}
	if _, ok := seenFrames[f]; ok {
		return false
	}
	seenFrames[f] = struct{}{}
	if f.cloneOf != nil {
		f.interp.funcMu.RLock()
		_, carrierRegistered := f.interp.funcMeta[f.funcCarrier]
		cloneOwnerFinished := f.cloneOf.funcState == funcFrameFinished
		f.interp.funcMu.RUnlock()
		if cloneOwnerFinished && !carrierRegistered {
			// The only remaining owner of this clone is its currently executing
			// wrapper. Once that call unwinds, values reachable only through the
			// clone cannot escape and must not be promoted to the root.
			return false
		}
	}

	f.mutex.RLock()
	data := append([]reflect.Value(nil), f.data...)
	f.mutex.RUnlock()
	visitor := funcValueVisitor{targets: targets}
	for _, value := range data {
		if visitor.contains(value) {
			return true
		}
	}
	return false
}

type funcValueVisitor struct {
	targets map[reflect.Value]struct{}
}

type funcValueMatch uint8

const (
	funcValueNoMatch funcValueMatch = iota
	funcValueExactMatch
	funcValueAmbiguousMatch
)

func (v *funcValueVisitor) contains(value reflect.Value) bool {
	return v.match(value) == funcValueExactMatch
}

func (v *funcValueVisitor) possiblyContains(value reflect.Value) bool {
	return v.match(value) != funcValueNoMatch
}

func (v *funcValueVisitor) match(value reflect.Value) funcValueMatch {
	if !value.IsValid() {
		return funcValueNoMatch
	}
	if value.CanInterface() {
		switch wrapped := value.Interface().(type) {
		case valueInterface:
			return v.match(wrapped.value)
		case reflect.Value:
			return v.match(wrapped)
		}
	}

	switch value.Kind() {
	case reflect.Func:
		if value.IsNil() {
			return funcValueNoMatch
		}
		key, ok := canonicalFuncValue(value)
		if !ok {
			return funcValueAmbiguousMatch
		}
		_, ok = v.targets[key]
		if ok {
			return funcValueExactMatch
		}
		return funcValueNoMatch
	case reflect.Interface:
		if value.IsNil() {
			return funcValueNoMatch
		}
		return v.match(value.Elem())
	case reflect.Ptr:
		if value.IsNil() || !typeMayContainFunc(value.Type().Elem(), map[reflect.Type]bool{}) {
			return funcValueNoMatch
		}
		// Native code can retain and mutate reference-backed values while an
		// interpreted call is canceled. Recursively reading them would race that
		// mutation even under the interpreter execution fence. Treat any non-nil,
		// function-capable reference as an ambiguous match for every candidate.
		return funcValueAmbiguousMatch
	case reflect.Struct:
		if !typeMayContainFunc(value.Type(), map[reflect.Type]bool{}) {
			return funcValueNoMatch
		}
		match := funcValueNoMatch
		for i := 0; i < value.NumField(); i++ {
			fieldMatch := v.match(value.Field(i))
			if fieldMatch == funcValueExactMatch {
				return fieldMatch
			}
			if fieldMatch == funcValueAmbiguousMatch {
				match = fieldMatch
			}
		}
		return match
	case reflect.Array:
		if !typeMayContainFunc(value.Type().Elem(), map[reflect.Type]bool{}) {
			return funcValueNoMatch
		}
		match := funcValueNoMatch
		for i := 0; i < value.Len(); i++ {
			elementMatch := v.match(value.Index(i))
			if elementMatch == funcValueExactMatch {
				return elementMatch
			}
			if elementMatch == funcValueAmbiguousMatch {
				match = elementMatch
			}
		}
		return match
	case reflect.Slice:
		if value.IsNil() || !typeMayContainFunc(value.Type().Elem(), map[reflect.Type]bool{}) {
			return funcValueNoMatch
		}
		return funcValueAmbiguousMatch
	case reflect.Map:
		if value.IsNil() || (!typeMayContainFunc(value.Type().Key(), map[reflect.Type]bool{}) &&
			!typeMayContainFunc(value.Type().Elem(), map[reflect.Type]bool{})) {
			return funcValueNoMatch
		}
		return funcValueAmbiguousMatch
	}
	return funcValueNoMatch
}

func typeMayContainFunc(typ reflect.Type, seen map[reflect.Type]bool) bool {
	if typ == nil {
		return false
	}
	if seen[typ] {
		return false
	}
	seen[typ] = true
	switch typ.Kind() {
	case reflect.Func, reflect.Interface:
		return true
	case reflect.Ptr, reflect.Array, reflect.Slice:
		return typeMayContainFunc(typ.Elem(), seen)
	case reflect.Map:
		return typeMayContainFunc(typ.Key(), seen) || typeMayContainFunc(typ.Elem(), seen)
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			if typeMayContainFunc(typ.Field(i).Type, seen) {
				return true
			}
		}
	}
	return false
}
