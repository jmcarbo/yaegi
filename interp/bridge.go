package interp

// The host bridge: native box types giving interpreted method sets a genuine
// native presence beyond interpreter boundaries.
//
// An interpreted value crossing into host code is represented either as a
// valueInterface wrapper (opaque to host code), as its raw concrete frame
// value (right shape for reflection, but its methods are invisible), or as a
// generated interface wrapper box: a struct whose first field (IValue) carries
// the interpreted value and whose remaining func fields are bound to the
// interpreted methods, with real promoted methods so the box genuinely
// implements the native interface (_error, _fmt_Stringer,
// _encoding_json_Marshaler, ...). Boxes are what let host code dispatch
// interpreted methods (fmt calls String, encoding/json calls MarshalJSON,
// errors.As accepts the value as an error).
//
// The catalog indexes every box type registered in the interpreter binary
// packages (extract-generated `_<Interface>` symbols, composed wrappers from
// mapTypes, and interp's own _error). It answers two questions:
//
//   - given an interpreted type (method set), which box can carry it into
//     host code (bridgeFor, used at the Eval API boundary);
//   - given a native value coming back into interpreted code, is it a box and
//     which interpreted value does it carry (unbridgeValue, used by type
//     assertions, method lookup and callback arguments).
//
// Inside interpreted programs, crossings into binary function parameters stay
// driven by mapTypes (per-function interface lists): a value that satisfies
// fmt.Stringer must not be boxed for encoding/json, which would reflect over
// the box instead of the struct and break structural marshalling.

import (
	"encoding"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// deepBridgePolicy registers, per binary function, the interface list and the
// direction of its container bridge. Read-only functions (the Marshal family)
// get their containers rebuilt with bridged elements; unmarshal-style
// functions get an inout mirror with write-back.
type deepBridgeSpec struct {
	lr    []reflect.Type
	inout bool
	// wrap marks functions which must receive error-implementing values as
	// genuine errors even though they cross an any slot (fmt.Errorf %w).
	wrap bool
}

var deepBridgePolicy = map[reflect.Value]deepBridgeSpec{}

func init() {
	marshalers := []reflect.Type{
		reflect.TypeOf((*json.Marshaler)(nil)).Elem(),
		reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem(),
		reflect.TypeOf((*xml.Marshaler)(nil)).Elem(),
	}
	unmarshalers := []reflect.Type{
		reflect.TypeOf((*json.Unmarshaler)(nil)).Elem(),
		reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem(),
		reflect.TypeOf((*xml.Unmarshaler)(nil)).Elem(),
	}
	deepBridgePolicy[reflect.ValueOf(json.Marshal)] = deepBridgeSpec{marshalers, false, false}
	deepBridgePolicy[reflect.ValueOf(json.MarshalIndent)] = deepBridgeSpec{marshalers, false, false}
	deepBridgePolicy[reflect.ValueOf(xml.Marshal)] = deepBridgeSpec{marshalers, false, false}
	deepBridgePolicy[reflect.ValueOf(xml.MarshalIndent)] = deepBridgeSpec{marshalers, false, false}
	deepBridgePolicy[reflect.ValueOf(json.Unmarshal)] = deepBridgeSpec{unmarshalers, true, false}
	// %w requires a genuine error: direct concrete values implementing
	// Error() string are boxed for this function only, so the other fmt
	// verbs keep formatting the raw value (a %d on a named int type, for
	// example, would otherwise see an opaque box).
	deepBridgePolicy[reflect.ValueOf(fmt.Errorf)] = deepBridgeSpec{
		[]reflect.Type{reflect.TypeOf((*error)(nil)).Elem()}, false, true}
}

// bridgeEntry describes one native box type able to carry an interpreted
// method set into host code as a genuine implementer of a native interface.
type bridgeEntry struct {
	box    reflect.Type   // box struct: IValue first field, one func field per method
	names  []string       // method names, in box field order
	fields []reflect.Type // method signatures (func), in box field order
}

// hostBridge is a host-registered bridge for an arbitrary host interface type:
// the host supplies the adapter factory turning an interpreted method caller
// into a genuine implementer.
type hostBridge struct {
	iface  reflect.Type
	names  []string
	fields []reflect.Type
	build  func(caller MethodCaller) (any, bool)
}

// bridgeState carries the per-interpreter bridge tables. Match caches are
// keyed by interpreted type id; the map presence flag distinguishes a cached
// miss (nil value) from an uncached type.
type bridgeState struct {
	mu          sync.RWMutex
	catalog     bridgeCatalog
	match       map[string]*bridgeEntry
	hostMatch   map[string]*hostBridge
	hostBridges []*hostBridge
}

// bridgeCatalog indexes the box types known to the interpreter.
type bridgeCatalog struct {
	entries []*bridgeEntry // most specific (most methods) first
	byBox   map[reflect.Type]*bridgeEntry
}

func (interp *Interpreter) bridgeState() *bridgeState {
	interp.bridgeOnce.Do(func() {
		interp.bridges = &bridgeState{
			match:     map[string]*bridgeEntry{},
			hostMatch: map[string]*hostBridge{},
		}
		// rebuildBridgeCatalog reads interp.bridges directly: calling
		// bridgeState here would re-enter sync.Once and deadlock.
		interp.rebuildBridgeCatalog()
	})
	return interp.bridges
}

// rebuildBridgeCatalog indexes all registered box types: symbols named
// `_<something>` in binary packages whose type is an interface wrapper struct
// (IValue first field + only func fields, with real promoted methods), plus
// composed wrapper types listed in mapTypes.
func (interp *Interpreter) rebuildBridgeCatalog() {
	b := interp.bridges
	if b == nil {
		return
	}
	entries := []*bridgeEntry{}
	byBox := map[reflect.Type]*bridgeEntry{}

	add := func(bt reflect.Type) {
		if bt == nil || byBox[bt] != nil {
			return
		}
		e := bridgeEntryOf(bt)
		if e == nil {
			return
		}
		entries = append(entries, e)
		byBox[bt] = e
	}

	for _, pkg := range interp.binPkg {
		for name, sym := range pkg {
			if !strings.HasPrefix(name, "_") || !sym.IsValid() {
				continue
			}
			t := sym.Type()
			if t.Kind() != reflect.Ptr {
				continue
			}
			add(t.Elem())
		}
	}
	// Composed wrappers are registered in mapTypes keyed by their type value.
	for k := range interp.mapTypes {
		if !k.IsValid() {
			continue
		}
		t := k.Type()
		if t.Kind() != reflect.Ptr || t.Elem().Kind() != reflect.Struct {
			continue
		}
		add(t.Elem())
	}
	// Most specific first: composed wrappers (more methods) before simple
	// ones, mirroring the mapTypes construction order. Ties are broken by a
	// preference for well-known interface packages, then by box type name:
	// the box chosen for a given method set is deterministic across runs
	// (binPkg iteration order is random), and a single-method type is
	// presented as fmt.Stringer rather than an obscure same-shape interface.
	sort.SliceStable(entries, func(i, j int) bool {
		if len(entries[i].names) != len(entries[j].names) {
			return len(entries[i].names) > len(entries[j].names)
		}
		pi, pj := boxPathRank(entries[i].box), boxPathRank(entries[j].box)
		if pi != pj {
			return pi < pj
		}
		return entries[i].box.String() < entries[j].box.String()
	})

	b.mu.Lock()
	defer b.mu.Unlock()
	b.catalog.entries = entries
	b.catalog.byBox = byBox
	b.match = map[string]*bridgeEntry{}
	b.hostMatch = map[string]*hostBridge{}
}

// boxPathRank orders interface packages by how canonical they are for
// host-facing boxing: well-known std interfaces first (lower rank).
func boxPathRank(t reflect.Type) int {
	path := t.PkgPath()
	if path == "" {
		// interp's own boxes (_error).
		return 0
	}
	if r, ok := boxPathRankTable[path]; ok {
		return r
	}
	return 100
}

var boxPathRankTable = map[string]int{
	"":                0,
	"fmt":             1,
	"errors":          1,
	"encoding":        2,
	"encoding/json":   2,
	"encoding/xml":    2,
	"sort":            3,
	"strconv":         3,
	"io":              3,
	"bufio":           4,
	"container/heap":  4,
	"container/list":  4,
	"container/ring":  4,
	"net/http":        4,
	"database/sql":    4,
	"compress/flate":  5,
	"compress/gzip":   5,
	"compress/zlib":   5,
	"compress/bzip2":  5,
	"compress/lzw":    5,
	"crypto":          5,
	"crypto/cipher":   5,
	"crypto/elliptic": 5,
	"crypto/ecdh":     5,
	"hash":            5,
	"expvar":          90,
}

// bridgeEntryOf validates that t is an interface wrapper box type and returns
// its catalog entry, or nil.
func bridgeEntryOf(t reflect.Type) *bridgeEntry {
	if !isInterfaceWrapperType(t) {
		return nil
	}
	// The box must genuinely implement a native interface: promoted methods
	// are what host code dispatches. Without them it is not a bridge.
	if t.NumMethod() == 0 {
		return nil
	}
	e := &bridgeEntry{box: t}
	for i := 1; i < t.NumField(); i++ {
		f := t.Field(i)
		// Every func field must be backed by a promoted method of the same
		// name: it is what native code actually calls. A box may carry
		// extra promoted methods beyond its fields (e.g. _error.As for
		// errors.As chains); they are self-contained and harmless.
		if _, ok := t.MethodByName(f.Name[1:]); !ok {
			return nil
		}
		e.names = append(e.names, f.Name[1:])
		e.fields = append(e.fields, f.Type)
	}
	if len(e.names) == 0 || t.NumMethod() < len(e.names) {
		return nil
	}
	return e
}

// bridgeFor returns the most specific catalog box the interpreted type can
// populate, or nil. A method matches when it resolves on the type with the
// signature the box field expects.
func (interp *Interpreter) bridgeFor(typ *itype) *bridgeEntry {
	if typ == nil || !mayHaveMethods(typ) {
		return nil
	}
	b := interp.bridgeState()
	id := typ.id()
	b.mu.RLock()
	e, seen := b.match[id]
	b.mu.RUnlock()
	if seen {
		return e
	}
	e = interp.matchBridge(typ, b.catalog.entries)
	b.mu.Lock()
	b.match[id] = e
	b.mu.Unlock()
	return e
}

// matchBridge finds the first entry (most specific first) whose method set is
// satisfied by typ.
func (interp *Interpreter) matchBridge(typ *itype, entries []*bridgeEntry) *bridgeEntry {
	if len(entries) == 0 {
		return nil
	}
	ms := typ.methods()
	if len(ms) == 0 {
		return nil
	}
	for _, e := range entries {
		if interp.bridgeMatches(typ, ms, e) {
			return e
		}
	}
	return nil
}

// mayHaveMethods is a cheap prefilter before building the full method set.
func mayHaveMethods(typ *itype) bool {
	switch typ.cat {
	case structT, ptrT, interfaceT, linkedT:
		return len(typ.method) > 0 || len(typ.field) > 0
	case valueT, errorT:
		return true
	}
	return false
}

// bridgeMatches reports whether typ resolves every method of the entry with
// the expected signature.
func (interp *Interpreter) bridgeMatches(typ *itype, ms methodSet, e *bridgeEntry) bool {
	for i, name := range e.names {
		if _, ok := ms[name]; !ok {
			return false
		}
		if !interp.bridgeMethodCompatible(typ, name, e.fields[i]) {
			return false
		}
	}
	return true
}

// bridgeMethodCompatible checks that the named method resolves on typ with the
// given native func signature.
func (interp *Interpreter) bridgeMethodCompatible(typ *itype, name string, want reflect.Type) bool {
	if m, _ := typ.lookupMethod(name); m != nil {
		return methodSignatureMatches(m, want)
	}
	// Binary methods (promoted from embedded binary values) carry the exact
	// native signature of their interface; presence is sufficient, binding
	// goes through methodByName at box build time.
	if _, _, _, ok := typ.lookupBinMethod(name); ok {
		return true
	}
	return false
}

// buildBridgeBox populates the box of entry e for the interpreted value v of
// type typ, within the live frame f. The IValue field carries a valueInterface
// (node + concrete value) so interpreted code can recognize and unbox the
// value later; host code ignores it and dispatches through the promoted
// methods. It returns ok false when a method cannot be bound.
func (interp *Interpreter) buildBridgeBox(typ *itype, e *bridgeEntry, v reflect.Value, f *frame) (reflect.Value, bool) {
	for i, name := range e.names {
		if !interp.bridgeMethodCompatible(typ, name, e.fields[i]) {
			return reflect.Value{}, false
		}
	}
	w := reflect.New(e.box).Elem()
	w.Field(0).Set(reflect.ValueOf(valueInterface{node: nodeOfType(typ), value: v}))
	for i, name := range e.names {
		r := bindBridgeMethod(typ, v, name, f)
		if !r.IsValid() {
			return reflect.Value{}, false
		}
		w.Field(i + 1).Set(r)
	}
	return w, true
}

// bindBridgeMethod binds the named method of typ on value v, mirroring the
// binding rules of genInterfaceWrapper: interpreted methods bind through a
// wrapper with the receiver set, binary methods through reflect MethodByName
// (extended to embedded fields and valueInterface wrappers).
func bindBridgeMethod(typ *itype, v reflect.Value, name string, f *frame) reflect.Value {
	if m, index := typ.lookupMethod(name); m != nil {
		nod := *m
		nod.val = &nod
		// A receiver with no node makes genValueRecv read the value directly,
		// instead of resolving it through a (non-existent) node cell.
		nod.recv = &receiver{val: v, index: index}
		return genFunctionWrapper(&nod)(f)
	}
	return methodByName(v, name, nil)
}

// nodeOfType returns a minimal node carrying the interpreted type, for
// receiver references where no source node is at hand.
func nodeOfType(typ *itype) *node {
	return &node{typ: typ}
}

// unbridgeValue recognizes a bridge box (or a legacy interface wrapper) and
// returns the interpreted view it carries: a valueInterface when the node is
// known, the concrete value otherwise. ok is false when v is not a box.
func unbridgeValue(v reflect.Value) (valueInterface, bool) {
	for v.IsValid() && v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return valueInterface{}, false
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return valueInterface{}, false
	}
	if _, ok := v.Interface().(valueInterface); ok {
		return valueInterface{}, false
	}
	if !isInterfaceWrapperType(v.Type()) {
		return valueInterface{}, false
	}
	iv := v.Field(0)
	if !iv.IsValid() || !iv.CanInterface() {
		return valueInterface{}, false
	}
	if c, ok := iv.Interface().(valueInterface); ok {
		return c, true
	}
	// Legacy boxes (genInterfaceWrapper) store the concrete value.
	if c := iv.Interface(); c != nil {
		if cv := reflect.ValueOf(c); cv.IsValid() {
			return valueInterface{value: cv}, true
		}
	}
	return valueInterface{value: iv.Elem()}, true
}

// RegisterBridge teaches the interpreter to present interpreted values as
// genuine implementers of the host interface type T at the API boundary. The
// build function receives a MethodCaller dispatching the interpreted method
// set and returns a value implementing T, or (zero, false) to decline.
//
// Bridges complement the built-in catalog (error, fmt.Stringer,
// json.Marshaler, ...): they are the only way to satisfy a host-declared
// interface, because Go cannot synthesize new method sets at runtime.
func RegisterBridge[T any](interp *Interpreter, build func(caller MethodCaller) (T, bool)) {
	var zero T
	iface := reflect.TypeOf(&zero).Elem()
	if iface.Kind() != reflect.Interface || iface.NumMethod() == 0 {
		panic("yaegi: RegisterBridge must be called with an interface type")
	}
	hb := &hostBridge{
		iface:  iface,
		names:  make([]string, 0, iface.NumMethod()),
		fields: make([]reflect.Type, 0, iface.NumMethod()),
	}
	for i := 0; i < iface.NumMethod(); i++ {
		m := iface.Method(i)
		hb.names = append(hb.names, m.Name)
		hb.fields = append(hb.fields, m.Type)
	}
	hb.build = func(caller MethodCaller) (any, bool) {
		v, ok := build(caller)
		if !ok {
			return nil, false
		}
		return v, true
	}
	b := interp.bridgeState()
	b.mu.Lock()
	b.hostBridges = append(b.hostBridges, hb)
	b.hostMatch = map[string]*hostBridge{}
	b.mu.Unlock()
}

// hostBridgeFor returns the registered host bridge whose interface the
// interpreted type satisfies, if any.
func (interp *Interpreter) hostBridgeFor(typ *itype) *hostBridge {
	if typ == nil {
		return nil
	}
	b := interp.bridgeState()
	id := typ.id()
	b.mu.RLock()
	hb, seen := b.hostMatch[id]
	b.mu.RUnlock()
	if seen {
		return hb
	}
	hb = interp.matchHostBridge(typ)
	b.mu.Lock()
	b.hostMatch[id] = hb
	b.mu.Unlock()
	return hb
}

func (interp *Interpreter) matchHostBridge(typ *itype) *hostBridge {
	b := interp.bridgeState()
	// Snapshot under the lock: registrations may append concurrently.
	b.mu.RLock()
	bridges := b.hostBridges
	b.mu.RUnlock()
	if len(bridges) == 0 || !mayHaveMethods(typ) {
		return nil
	}
	ms := typ.methods()
	for _, cand := range bridges {
		if len(ms) < len(cand.names) {
			continue
		}
		ok := true
		for i, name := range cand.names {
			if _, present := ms[name]; !present {
				ok = false
				break
			}
			if !interp.bridgeMethodCompatible(typ, name, cand.fields[i]) {
				ok = false
				break
			}
		}
		if ok {
			return cand
		}
	}
	return nil
}

// hostBridgeValue builds the registered host adapter for v, within the live
// frame f. Method wrappers are bound eagerly while the frame is alive, so the
// adapter keeps dispatching interpreted methods after the evaluation ends,
// like any function value returned to host code. ok is false when no
// registered bridge matches or the adapter declines.
func (interp *Interpreter) hostBridgeValue(typ *itype, v reflect.Value, f *frame) (reflect.Value, bool) {
	hb := interp.hostBridgeFor(typ)
	if hb == nil {
		return reflect.Value{}, false
	}
	wrappers := make(map[string]reflect.Value, len(hb.names))
	for _, name := range hb.names {
		w := bindBridgeMethod(typ, v, name, f)
		if !w.IsValid() {
			return reflect.Value{}, false
		}
		wrappers[name] = w
	}
	adapter, ok := hb.build(&methodCaller{wrappers: wrappers})
	if !ok {
		return reflect.Value{}, false
	}
	av := reflect.ValueOf(adapter)
	if !av.IsValid() || !av.Type().Implements(hb.iface) {
		return reflect.Value{}, false
	}
	return av, true
}

// MethodCaller dispatches the prebuilt interpreted methods of one bridged
// value to host code.
type MethodCaller interface {
	// CallMethod invokes the named interpreted method on the bridged value
	// with native arguments and returns its native results. It fails when
	// the method was not bound for this value.
	CallMethod(name string, in []reflect.Value) ([]reflect.Value, error)
}

type methodCaller struct {
	wrappers map[string]reflect.Value
}

func (mc *methodCaller) CallMethod(name string, in []reflect.Value) ([]reflect.Value, error) {
	w, ok := mc.wrappers[name]
	if !ok {
		return nil, &BridgeMethodError{Name: name}
	}
	return w.Call(in), nil
}

// BridgeMethodError reports a method missing from a bridged interpreted
// method set.
type BridgeMethodError struct{ Name string }

func (e *BridgeMethodError) Error() string { return "yaegi: bridged value has no method " + e.Name }

// methodSignatureMatches reports whether the interpreted method node m has
// the native signature want. Method declaration nodes carry their receiver
// separately (def.typ.recv), but some signatures still include it as the
// first argument, so both forms are accepted.
func methodSignatureMatches(m *node, want reflect.Type) bool {
	got := m.typ.TypeOf()
	if got == nil || got.Kind() != reflect.Func {
		return false
	}
	if got == want {
		return true
	}
	if got.NumOut() != want.NumOut() || got.IsVariadic() != want.IsVariadic() {
		return false
	}
	if got.NumIn() == want.NumIn() && funcOfEqual(got, want) {
		return true
	}
	if got.NumIn() == want.NumIn()+1 {
		// Receiver included as first argument: strip it.
		in := make([]reflect.Type, want.NumIn())
		for i := range in {
			in[i] = got.In(i + 1)
		}
		out := make([]reflect.Type, want.NumOut())
		for i := range out {
			out[i] = got.Out(i)
		}
		return reflect.FuncOf(in, out, want.IsVariadic()) == want
	}
	return false
}

func funcOfEqual(a, b reflect.Type) bool {
	if a == b {
		return true
	}
	if a.NumIn() != b.NumIn() || a.NumOut() != b.NumOut() || a.IsVariadic() != b.IsVariadic() {
		return false
	}
	for i := 0; i < a.NumIn(); i++ {
		if a.In(i) != b.In(i) {
			return false
		}
	}
	for i := 0; i < a.NumOut(); i++ {
		if a.Out(i) != b.Out(i) {
			return false
		}
	}
	return true
}

// bridgeEvalResult re-boxes an interpreted-interface result crossing the Eval
// API boundary. The host must never receive the valueInterface wrapper: it is
// unexported, opaque, and satisfies no host interface. The interpreted cell is
// untouched; interpreted reads and method dispatch keep operating on it.
func (interp *Interpreter) bridgeEvalResult(res reflect.Value, f *frame) reflect.Value {
	if !res.IsValid() || res.Type() != valueInterfaceType {
		return res
	}
	vi, ok := res.Interface().(valueInterface)
	if !ok || vi.node == nil || vi.node.typ == nil || !vi.value.IsValid() {
		// Zero value or type symbol edge: keep the historical representation.
		return res
	}
	switch vi.value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		if vi.value.IsNil() {
			return vi.value
		}
	}
	if isEmptyInterface(vi.node.typ) {
		// An empty-interface holder carries no method set: the concrete
		// value is the whole story.
		return vi.value
	}
	if box := interp.bridgeBoxForView(vi.node.typ, vi.value, f); box.IsValid() {
		return box
	}
	return vi.value
}

// bridgeEvalResultRaw recovers and re-boxes the content of a binary-typed
// (any) result cell: values unwrapped for binary crossings carry no method,
// so an interpreted method set is restored through the rtype reverse map and
// presented to the host through a native box when one matches. Method-less
// and native values are returned unchanged: raw structural reflection over
// plain data keeps working.
func (interp *Interpreter) bridgeEvalResultRaw(res reflect.Value, f *frame) reflect.Value {
	if !res.IsValid() || res.Kind() != reflect.Interface || res.IsNil() {
		return res
	}
	content := res.Elem()
	if !content.IsValid() || content.Type() == valueInterfaceType {
		return res
	}
	vi, _, ok := interp.recoverInterpretedView(content)
	if !ok || vi.node == nil {
		return res
	}
	if box := interp.bridgeBoxForView(vi.node.typ, vi.value, f); box.IsValid() {
		return box
	}
	return res
}

var (
	errorReflectType    = reflect.TypeOf((*error)(nil)).Elem()
	errorCellType       = reflect.TypeOf(_errorCell{})
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

// isOutErrorBridge decides whether a pointer-to-pointer argument crossing a
// binary any slot must be replaced by an out-parameter error bridge (the
// errors.As family): the pointee's interpreted method set must satisfy the
// native error interface per the callee's interface list.
func isOutErrorBridge(getMapType func(*itype) reflect.Type, n *node) bool {
	if getMapType == nil {
		return false
	}
	return getMapType(n.typ.val) == errorReflectType
}

// genValueOutErrorBridge builds the out-parameter bridge for &target where
// target is an interpreted variable of a concrete error-implementing type:
// the binary call receives a pointer to a fresh native _errorCell box whose
// Want is the native type of the target cell. The current value is seeded in
// the box so an unmatched call writes the same value back (a no-op).
func genValueOutErrorBridge(n *node) func(*frame) reflect.Value {
	cell := genValue(n.child[0]) // the target variable cell, holds *T values
	want := n.typ.val.TypeOf()   // the *T native type of the cell content
	return func(f *frame) reflect.Value {
		var carried any
		if cur := cell(f); cur.IsValid() && cur.Kind() == reflect.Ptr && !cur.IsNil() {
			carried = cur.Interface()
		}
		// The whole value is set at once: reflecting into the unexported
		// embedded _error field would poison the box value.
		box := reflect.ValueOf(_errorCell{_error: _error{IValue: carried}, Want: want})
		out := reflect.New(errorCellType).Elem()
		out.Set(box)
		return out.Addr()
	}
}

// writeBackOutErrorBridge moves the matched error back into the interpreted
// target cell after the binary call returns. errors.As stores the matched
// chain link in the box (through _error.As); the write-back unboxes it to the
// concrete value the cell expects. An unmatched call still carries the seeded
// original value, so the write is a semantic no-op.
func writeBackOutErrorBridge(n *node) func(*frame, reflect.Value) {
	cell := genValue(n.child[0])
	return func(f *frame, passed reflect.Value) {
		if !passed.IsValid() || passed.Kind() != reflect.Ptr || passed.IsNil() {
			return
		}
		box := passed.Elem()
		if !box.IsValid() || box.Type() != errorCellType {
			return
		}
		// Read the carried value through a native type assertion: reflect
		// field access would cross the unexported embedded field.
		cv, ok := box.Interface().(_errorCell)
		if !ok || cv.IValue == nil {
			return
		}
		iv := reflect.Value{}
		if vi, ok2 := cv.IValue.(valueInterface); ok2 {
			iv = vi.value
		} else {
			iv = reflect.ValueOf(cv.IValue)
		}
		if !iv.IsValid() {
			return
		}
		dest := cell(f)
		if !dest.IsValid() || !dest.CanSet() || dest.Type() != iv.Type() {
			return
		}
		f.interp.markOwnedCellWriteFromExec(dest, iv)
		dest.Set(iv)
	}
}

// recoverInterpretedView recovers the interpreted view of a value stored in a
// binary-typed cell: a valueInterface wrapper is traversed, a bridge box is
// unboxed, and a raw concrete value is traced back to its interpreted type
// through the rtype reverse map (values are unwrapped for binary empty
// interface parameters, so their method set is invisible to native code).
// The returned reflect.Value re-boxes the view for source cells which must
// keep carrying a valueInterface.
func (interp *Interpreter) recoverInterpretedView(v reflect.Value) (valueInterface, reflect.Value, bool) {
	for v.IsValid() && v.Kind() == reflect.Interface {
		if v.IsNil() {
			return valueInterface{}, v, false
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return valueInterface{}, v, false
	}
	if vi, ok := v.Interface().(valueInterface); ok {
		return vi, reflect.ValueOf(vi), true
	}
	if vi, ok := unbridgeValue(v); ok {
		return vi, reflect.ValueOf(vi), true
	}
	if v.Kind() != reflect.Struct && v.Kind() != reflect.Ptr {
		return valueInterface{}, v, false
	}
	t := interp.rtypeItype(v.Type())
	if t == nil || isEmptyInterface(t) || !mayHaveMethods(t) {
		return valueInterface{}, v, false
	}
	vi := valueInterface{node: nodeOfType(t), value: v}
	return vi, reflect.ValueOf(vi), true
}

// bridgeBoxForView returns the richest native box for an interpreted view:
// a host-registered bridge when one matches, else a catalog box, else an
// invalid Value.
func (interp *Interpreter) bridgeBoxForView(typ *itype, v reflect.Value, f *frame) reflect.Value {
	if box, ok := interp.hostBridgeValue(typ, v, f); ok {
		return box
	}
	if e := interp.bridgeFor(typ); e != nil {
		if box, ok := interp.buildBridgeBox(typ, e, v, f); ok {
			return box
		}
	}
	return reflect.Value{}
}

// bridgeHostResults presents interpreted results crossing to host code
// through a function wrapper in their richest native form, like the Eval API
// boundary does: a valueInterface-typed result cell or an any-typed cell
// carrying an interpreted concrete value is replaced by a native bridge box
// when the value's method set satisfies a registered host interface or a
// catalog interface. Method-less and native results are returned unchanged.
func (interp *Interpreter) bridgeHostResults(out []reflect.Value, root *frame) {
	for i, r := range out {
		if !r.IsValid() {
			continue
		}
		if r.Type() == valueInterfaceType {
			if vi, ok := r.Interface().(valueInterface); ok && vi.node != nil && vi.node.typ != nil && vi.value.IsValid() {
				if isEmptyInterface(vi.node.typ) {
					out[i] = vi.value
					continue
				}
				if box := interp.bridgeBoxForView(vi.node.typ, vi.value, root); box.IsValid() {
					out[i] = box
				}
			}
			continue
		}
		if r.Kind() != reflect.Interface || r.IsNil() {
			continue
		}
		content := r.Elem()
		if !content.IsValid() || content.Type() == valueInterfaceType || content.Kind() != reflect.Struct {
			continue
		}
		vi, _, ok := interp.recoverInterpretedView(content)
		if !ok || vi.node == nil {
			continue
		}
		if box := interp.bridgeBoxForView(vi.node.typ, vi.value, root); box.IsValid() {
			out[i] = box
		}
	}
}

// Deep bridge: per-element interface dispatch for containers crossing binary
// functions which only READ their any arguments (json.Marshal,
// json.Encoder.Encode, ...). A container crossing the unwrap policy carries
// raw concrete elements whose interpreted methods are invisible to native
// reflection: encoding/json then skips per-element Marshaler dispatch. The
// deep bridge rebuilds the container shape, boxing the elements (or the
// pointee of addressable elements) which satisfy one of the callee's
// interfaces, so native code dispatches interpreted methods per element, as
// native Go would.
//
// The bridge is restricted to functions registered in mapTypes: their
// interface lists define what may be boxed. Boxing a fmt.Stringer for a
// json.Marshal call, for example, would replace the structural reflection of
// the element with an opaque box and break marshalling; keeping the lists
// per function preserves the read-only contract of the crossing.

// getDeepMapType returns, for the callee's interface list, a function
// checking whether an interpreted type is a container whose contents satisfy
// one of the interfaces (directly, or through a pointer for addressable
// elements). It is nil when the callee has no interface list.
func getDeepMapType(lr []reflect.Type) func(*itype) reflect.Type {
	if len(lr) == 0 {
		return nil
	}
	return func(typ *itype) reflect.Type {
		return deepBridgeMatch(typ, lr, 0)
	}
}

// deepBridgeMatch returns the interface satisfied by the contents of typ, or
// nil. Only container shapes are considered: the element itself crossing an
// any slot is the realm of the top-level mapTypes wrap.
func deepBridgeMatch(typ *itype, lr []reflect.Type, depth int) reflect.Type {
	if typ == nil || depth > deepBridgeMaxDepth {
		return nil
	}
	switch typ.cat {
	case sliceT, arrayT, variadicT:
		return firstSatisfied(typ.val, lr, true)
	case mapT:
		return firstSatisfied(typ.val, lr, false)
	case ptrT:
		return deepBridgeMatch(typ.val, lr, depth+1)
	case structT:
		for _, fld := range typ.field {
			if rt := deepBridgeMatch(fld.typ, lr, depth+1); rt != nil {
				return rt
			}
		}
	}
	return nil
}

const deepBridgeMaxDepth = 16

// firstSatisfied returns the first interface of lr implemented by typ (or by
// *typ when addressable, mirroring the native addressability rules for
// pointer-receiver methods).
func firstSatisfied(typ *itype, lr []reflect.Type, addressable bool) reflect.Type {
	if typ == nil {
		return nil
	}
	for _, rt := range lr {
		if typ.implements(&itype{cat: valueT, rtype: rt}) {
			return rt
		}
		if addressable {
			pt := &itype{cat: ptrT, val: typ}
			if pt.implements(&itype{cat: valueT, rtype: rt}) {
				return rt
			}
		}
	}
	return nil
}

// genValueDeepBridge rebuilds the container value with bridged elements. lr
// is the callee's interface list; typ is the static interpreted type of the
// argument.
func genValueDeepBridge(n *node, typ *itype, lr []reflect.Type) func(*frame) reflect.Value {
	value := genValue(n)
	return func(f *frame) reflect.Value {
		v := value(f)
		if !v.IsValid() {
			return v
		}
		concrete := valueInterfaceValue(v)
		if !concrete.IsValid() {
			return v
		}
		// An internal bridge failure degrades to the raw crossing instead
		// of crashing the interpreted program.
		nv, changed, bridged := func() (rv reflect.Value, changed, ok bool) {
			defer func() {
				if r := recover(); r != nil {
					rv, changed, ok = v, false, false
				}
			}()
			rv, ch := deepBridgeValue(f, concrete, typ, lr, 0, true)
			return rv, ch, true
		}()
		if !bridged || !changed {
			return v
		}
		return nv
	}
}

var (
	deepBridgeIfaceType = reflect.TypeOf((*interface{})(nil)).Elem()
	deepBridgeSliceType = reflect.SliceOf(deepBridgeIfaceType)
)

// deepBridgeValue walks a container value alongside its interpreted type and
// rebuilds it with boxes where elements satisfy the interface list. It
// reports whether anything changed.
func deepBridgeValue(f *frame, v reflect.Value, typ *itype, lr []reflect.Type, depth int, addressable bool) (reflect.Value, bool) {
	if depth > deepBridgeMaxDepth {
		return v, false
	}
	if rt := firstSatisfied(typ, lr, addressable && v.CanAddr()); rt != nil {
		if box, ok := f.interp.buildBoxForInterface(typ, v, rt, f, addressable); ok {
			return box, true
		}
		return v, false
	}
	switch typ.cat {
	case sliceT, variadicT:
		if v.Kind() != reflect.Slice || v.Len() == 0 || (v.IsNil() && v.Len() == 0) {
			return v, false
		}
		any := false
		out := reflect.MakeSlice(deepBridgeSliceType, v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			ev, changed := deepBridgeValue(f, v.Index(i), typ.val, lr, depth+1, true)
			if changed {
				any = true
			}
			out.Index(i).Set(ev)
		}
		if !any {
			return v, false
		}
		return out, true
	case arrayT:
		if v.Kind() != reflect.Array || v.Len() == 0 {
			return v, false
		}
		any := false
		out := reflect.MakeSlice(deepBridgeSliceType, v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			ev, changed := deepBridgeValue(f, v.Index(i), typ.val, lr, depth+1, true)
			if changed {
				any = true
			}
			out.Index(i).Set(ev)
		}
		if !any {
			return v, false
		}
		return out, true
	case mapT:
		if v.Kind() != reflect.Map || v.Len() == 0 {
			return v, false
		}
		any := false
		out := reflect.MakeMapWithSize(reflect.MapOf(v.Type().Key(), deepBridgeIfaceType), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			mv, changed := deepBridgeValue(f, iter.Value(), typ.val, lr, depth+1, false)
			if changed {
				any = true
			}
			out.SetMapIndex(iter.Key(), mv)
		}
		if !any {
			return v, false
		}
		return out, true
	case structT:
		if v.Kind() != reflect.Struct {
			return v, false
		}
		out, changed := deepBridgeStruct(f, v, typ, lr, depth)
		if !changed {
			return v, false
		}
		return out, true
	case ptrT:
		if v.Kind() != reflect.Ptr || v.IsNil() {
			return v, false
		}
		pv, changed := deepBridgeValue(f, v.Elem(), typ.val, lr, depth+1, true)
		if !changed {
			return v, false
		}
		p := reflect.New(pv.Type())
		p.Elem().Set(pv)
		return p, true
	}
	return v, false
}

var deepBridgeStructTypes sync.Map // reflect.Type -> rebuilt reflect.Type

// deepBridgeStruct rebuilds a struct whose bridged fields (typically slices
// of marshaler elements) are widened to interface{}, preserving names, tags
// and order so native reflection over the rebuilt struct produces the same
// JSON shape. The rebuilt type is cached per original type.
func deepBridgeStruct(f *frame, v reflect.Value, typ *itype, lr []reflect.Type, depth int) (reflect.Value, bool) {
	if cached, ok := deepBridgeStructTypes.Load(v.Type()); ok {
		out := reflect.New(cached.(reflect.Type)).Elem()
		deepBridgeStructCopy(f, v, out, typ, lr, depth)
		return out, true
	}
	fields := make([]reflect.StructField, v.NumField())
	bridge := make([]bool, v.NumField())
	any := false
	for i := range fields {
		src := v.Type().Field(i)
		fields[i] = reflect.StructField{Name: src.Name, Type: src.Type, Tag: src.Tag, Anonymous: src.Anonymous}
		if i < len(typ.field) {
			if ft := typ.field[i].typ; ft != nil {
				if rt := deepBridgeMatch(ft, lr, depth+1); rt != nil {
					fields[i].Type = deepBridgeIfaceType
					bridge[i] = true
					any = true
				}
			}
		}
	}
	if !any {
		return v, false
	}
	outType := reflect.StructOf(fields)
	deepBridgeStructTypes.Store(v.Type(), outType)
	out := reflect.New(outType).Elem()
	deepBridgeStructCopy(f, v, out, typ, lr, depth)
	return out, true
}

// deepBridgeStructCopy fills a rebuilt struct from the original, bridging the
// widened fields.
func deepBridgeStructCopy(f *frame, v, out reflect.Value, typ *itype, lr []reflect.Type, depth int) {
	for i := 0; i < out.NumField(); i++ {
		if out.Type().Field(i).Type == deepBridgeIfaceType && i < len(typ.field) {
			fv, changed := deepBridgeValue(f, v.Field(i), typ.field[i].typ, lr, depth+1, v.Field(i).CanAddr())
			if changed {
				out.Field(i).Set(fv)
				continue
			}
		}
		if out.Field(i).CanSet() && v.Field(i).Type() == out.Field(i).Type() {
			out.Field(i).Set(v.Field(i))
		}
	}
}

// buildBoxForInterface boxes v (of interpreted type typ) as an implementer of
// the native interface rt, binding the interpreted methods. It is the
// value-based counterpart of genInterfaceWrapper for elements which carry no
// AST node.
func (interp *Interpreter) buildBoxForInterface(typ *itype, v reflect.Value, rt reflect.Type, f *frame, addressable bool) (reflect.Value, bool) {
	syn := &node{typ: typ, interp: interp}
	wrap := getWrapper(syn, rt)
	if wrap == nil {
		return reflect.Value{}, false
	}
	mn := wrap.NumField() - 1
	w := reflect.New(wrap).Elem()
	w.Field(0).Set(v)
	for i := 0; i < mn; i++ {
		name := wrap.Field(i + 1).Name[1:]
		r := bindBridgeMethod(typ, v, name, f)
		if !r.IsValid() && addressable && v.CanAddr() {
			// Pointer-receiver methods bind on the addressable element.
			if m, index := typ.lookupMethod(name); m != nil && isPtrRecvMethod(m) {
				nod := *m
				nod.val = &nod
				nod.recv = &receiver{syn, v.Addr(), index}
				r = genFunctionWrapper(&nod)(f)
			}
		}
		if !r.IsValid() {
			return reflect.Value{}, false
		}
		w.Field(i + 1).Set(r)
	}
	return w, true
}

// Inout deep bridge: write side of the container bridge (json.Unmarshal and
// friends). Native code cannot dispatch interpreted UnmarshalJSON per element
// through a raw interpreted container, and a mirror typed with bridge boxes
// cannot survive json-allocated elements. The mirror therefore widens only
// the leaves which satisfy the callee interfaces to interface{} (everything
// else keeps its native type, so json fills it structurally), and the
// write-back routes each decoded generic leaf through the interpreted
// UnmarshalJSON of the original element, which writes the original cell.

// genValueInOutDeepBridge substitutes a pointer-to-container argument with a
// pointer to a native mirror; the returned write-back moves decoded data into
// the original interpreted container.
func genValueInOutDeepBridge(n *node, typ *itype, lr []reflect.Type) (func(*frame) reflect.Value, func(*frame, reflect.Value)) {
	cell := genValue(n.child[0]) // pointer to the interpreted container
	pointee := typ.val
	return func(f *frame) reflect.Value {
			defer func() {
				if r := recover(); r != nil {
					panic(r)
				}
			}()
			cur := cell(f)
			if !cur.IsValid() {
				return cur
			}
			if cur.Kind() == reflect.Ptr {
				// A pointer variable: mirror its content.
				if cur.IsNil() {
					return cur
				}
				mirror := deepMirrorValue(f, cur.Elem(), pointee, lr, 0)
				return mirror.Addr()
			}
			// An addressable container cell: &col2 reads as a pointer into
			// the frame, the cell itself is the container.
			mirror := deepMirrorValue(f, cur, pointee, lr, 0)
			return mirror.Addr()
		}, func(f *frame, passed reflect.Value) {
			defer func() {
				if r := recover(); r != nil {
					panic(r)
				}
			}()
			// passed is the pointer to the mirror handed to the binary call.
			m := passed
			for m.IsValid() && (m.Kind() == reflect.Ptr || m.Kind() == reflect.Interface) {
				if m.IsNil() {
					return
				}
				m = m.Elem()
			}
			if !m.IsValid() {
				return
			}
			o := cell(f)
			if !o.IsValid() {
				return
			}
			if o.Kind() == reflect.Ptr {
				if o.IsNil() {
					return
				}
				o = o.Elem()
			}
			deepMirrorWriteBack(f, m, o, pointee, lr, 0)
		}
}

// deepMirrorValue builds a native mirror of v: containers rebuilt, leaves
// satisfying lr widened to interface{}, other leaves copied as native values.
func deepMirrorValue(f *frame, v reflect.Value, typ *itype, lr []reflect.Type, depth int) reflect.Value {
	if depth > deepBridgeMaxDepth || !v.IsValid() {
		return v
	}
	if rt := firstSatisfied(typ, lr, v.CanAddr()); rt != nil {
		// Widened leaf: seed with the current value, json replaces it.
		return reflect.ValueOf(v.Interface())
	}
	switch typ.cat {
	case sliceT, variadicT:
		if v.Kind() != reflect.Slice {
			return v
		}
		out := reflect.New(deepBridgeSliceType).Elem()
		out.Set(reflect.MakeSlice(deepBridgeSliceType, v.Len(), v.Cap()))
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(reflect.ValueOf(deepMirrorValue(f, v.Index(i), typ.val, lr, depth+1).Interface()))
		}
		return out
	case mapT:
		if v.Kind() != reflect.Map {
			return v
		}
		out := reflect.New(reflect.MapOf(v.Type().Key(), deepBridgeIfaceType)).Elem()
		out.Set(reflect.MakeMapWithSize(out.Type(), v.Len()))
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), reflect.ValueOf(deepMirrorValue(f, iter.Value(), typ.val, lr, depth+1).Interface()))
		}
		return out
	case ptrT:
		if v.Kind() != reflect.Ptr || v.IsNil() {
			return reflect.Zero(deepBridgeIfaceType)
		}
		inner := deepMirrorValue(f, v.Elem(), typ.val, lr, depth+1)
		p := reflect.New(inner.Type())
		p.Elem().Set(inner)
		return p
	case structT:
		if v.Kind() != reflect.Struct {
			return reflect.ValueOf(v.Interface())
		}
		return deepMirrorStruct(f, v, typ, lr, depth)
	}
	return reflect.ValueOf(v.Interface())
}

// deepMirrorStruct mirrors a struct, widening the fields whose contents
// satisfy the interface list. Field names, tags and order are preserved.
func deepMirrorStruct(f *frame, v reflect.Value, typ *itype, lr []reflect.Type, depth int) reflect.Value {
	fields := make([]reflect.StructField, v.NumField())
	widen := make([]bool, v.NumField())
	for i := range fields {
		src := v.Type().Field(i)
		fields[i] = reflect.StructField{Name: src.Name, Type: src.Type, Tag: src.Tag, Anonymous: src.Anonymous}
		if i < len(typ.field) && typ.field[i].typ != nil {
			if ft := typ.field[i].typ; firstSatisfiedContainer(ft, lr) {
				fields[i].Type = deepBridgeIfaceType
				widen[i] = true
			}
		}
	}
	out := reflect.New(reflect.StructOf(fields)).Elem()
	for i := range fields {
		if !out.Field(i).CanSet() {
			continue
		}
		if widen[i] {
			out.Field(i).Set(reflect.ValueOf(deepMirrorValue(f, v.Field(i), typ.field[i].typ, lr, depth+1).Interface()))
			continue
		}
		if v.Field(i).Type() == out.Field(i).Type() {
			out.Field(i).Set(v.Field(i))
		}
	}
	return out
}

// firstSatisfiedContainer reports whether the contents of typ satisfy one of
// the interfaces (the widening criterion for mirror fields).
func firstSatisfiedContainer(typ *itype, lr []reflect.Type) bool {
	return deepBridgeMatch(typ, lr, 0) != nil
}

// deepMirrorWriteBack moves the decoded mirror data into the original
// interpreted container. Widened leaves are routed through the interpreted
// method of the interface list (re-marshalling the generic value); native
// leaves are copied back directly.
func deepMirrorWriteBack(f *frame, mirror, orig reflect.Value, typ *itype, lr []reflect.Type, depth int) {
	if depth > deepBridgeMaxDepth || !orig.IsValid() {
		return
	}
	// Widened leaves and containers arrive boxed in interface{}: unwrap them
	// to the decoded value.
	for mirror.IsValid() && mirror.Kind() == reflect.Interface {
		if mirror.IsNil() {
			return
		}
		mirror = mirror.Elem()
	}
	if !mirror.IsValid() {
		return
	}
	if firstSatisfied(typ, lr, orig.CanAddr()) != nil {
		deepBridgeRouteMethod(f, mirror, orig, typ, lr)
		return
	}
	if !orig.CanSet() {
		return
	}
	switch typ.cat {
	case sliceT, variadicT:
		if mirror.Kind() != reflect.Slice {
			return
		}
		out := reflect.MakeSlice(orig.Type(), mirror.Len(), mirror.Cap())
		for i := 0; i < mirror.Len(); i++ {
			ev := out.Index(i)
			if i < orig.Len() {
				ev.Set(orig.Index(i))
			} else {
				ev.Set(reflect.Zero(orig.Type().Elem()))
			}
			deepMirrorWriteBack(f, mirror.Index(i), ev, typ.val, lr, depth+1)
		}
		f.interp.markOwnedCellWriteFromExec(orig, out)
		orig.Set(out)
	case mapT:
		if mirror.Kind() != reflect.Map {
			return
		}
		if orig.IsNil() {
			// json decoding into a nil map allocates it, as native does.
			orig.Set(reflect.MakeMap(orig.Type()))
		}
		iter := mirror.MapRange()
		for iter.Next() {
			// A settable scratch: the routed leaf writes into it.
			ev := reflect.New(orig.Type().Elem()).Elem()
			if cur := orig.MapIndex(iter.Key()); cur.IsValid() {
				ev.Set(cur)
			}
			deepMirrorWriteBack(f, iter.Value(), ev, typ.val, lr, depth+1)
			orig.SetMapIndex(iter.Key(), ev)
		}
	case ptrT:
		if !mirror.IsValid() || mirror.Kind() != reflect.Ptr || mirror.IsNil() {
			if mirror.IsValid() && mirror.Kind() == reflect.Ptr && mirror.IsNil() {
				orig.Set(reflect.Zero(orig.Type()))
			}
			return
		}
		if orig.IsNil() {
			orig.Set(reflect.New(orig.Type().Elem()))
		}
		deepMirrorWriteBack(f, mirror.Elem(), orig.Elem(), typ.val, lr, depth+1)
	case structT:
		// The mirror struct may have widened (interface{}) fields, so its
		// type differs from the original: iterate positionally.
		if mirror.Kind() != reflect.Struct || mirror.NumField() != orig.NumField() {
			return
		}
		for i := 0; i < mirror.NumField(); i++ {
			if i >= len(typ.field) {
				continue
			}
			deepMirrorWriteBack(f, mirror.Field(i), orig.Field(i), typ.field[i].typ, lr, depth+1)
		}
	default:
		if mirror.Type() == orig.Type() {
			f.interp.markOwnedCellWriteFromExec(orig, mirror)
			orig.Set(mirror)
		}
	}
}

// deepBridgeRouteMethod feeds one decoded generic leaf through the
// interpreted method of the first satisfied interface, targeting the original
// element cell. When the leaf is a native-typed value equal to the element
// (an untouched seed), it is assigned directly.
func deepBridgeRouteMethod(f *frame, mirror, orig reflect.Value, typ *itype, lr []reflect.Type) {
	if !orig.CanSet() {
		return
	}
	if mirror.IsValid() && mirror.Type() == orig.Type() {
		orig.Set(mirror)
		return
	}
	rt := firstSatisfied(typ, lr, true)
	if rt == nil || !mirror.IsValid() {
		return
	}
	// The decoded generic leaf is routed through the interpreted method in
	// the representation the interface expects: JSON bytes for
	// json.Unmarshaler, the bare string for TextUnmarshaler (re-marshalling
	// would keep the JSON quotes).
	var data []byte
	switch rt {
	case textUnmarshalerType:
		if mirror.Kind() == reflect.String {
			data = []byte(mirror.String())
		} else {
			var err error
			if data, err = jsonMarshalValue(mirror); err != nil {
				return
			}
		}
	default:
		var err error
		if data, err = jsonMarshalValue(mirror); err != nil {
			return
		}
	}
	methodName := interfaceMethodName(rt)
	if methodName == "" {
		return
	}
	m, index := typ.lookupMethod(methodName)
	if m == nil {
		return
	}
	// The interpreted method writes through the original cell: allocate
	// nil pointer elements (as native json does) and bind the receiver to
	// the cell itself (its address for pointer-receiver methods), so
	// mutations land in the interpreted container.
	if orig.Kind() == reflect.Ptr && orig.IsNil() {
		orig.Set(reflect.New(orig.Type().Elem()))
	}
	recv := orig
	if isPtrRecvMethod(m) && orig.Kind() != reflect.Ptr {
		recv = orig.Addr()
	}
	nod := *m
	nod.val = &nod
	nod.recv = &receiver{val: recv, index: index}
	w := genFunctionWrapper(&nod)(f)
	if !w.IsValid() {
		return
	}
	w.Call([]reflect.Value{reflect.ValueOf(data)})
}

// interfaceMethodName returns the single method of a single-method interface,
// or the Unmarshal-prefixed one for larger interfaces.
func interfaceMethodName(rt reflect.Type) string {
	if rt.NumMethod() == 1 {
		return rt.Method(0).Name
	}
	for i := 0; i < rt.NumMethod(); i++ {
		if strings.HasPrefix(rt.Method(i).Name, "Unmarshal") {
			return rt.Method(i).Name
		}
	}
	return ""
}

// jsonMarshalValue marshals a decoded generic value back to JSON, using
// encoding/json through its binary symbol to avoid an import cycle.
func jsonMarshalValue(v reflect.Value) ([]byte, error) {
	m, _ := jsonMarshalFunc.Load().(func(any) ([]byte, error))
	if m == nil {
		return nil, errors.New("yaegi: no json marshaler registered")
	}
	return m(v.Interface())
}

// jsonMarshalFunc is armed by Use of the encoding/json symbols.
var jsonMarshalFunc atomic.Value

// genPtrAliasInterfaceWrapper wraps an interpreted pointer whose pointee
// satisfies the parameter interface, passing a pointer to the box so native
// code (errors.As, json.Unmarshal) finds an addressable implementer whose
// methods write through to the interpreted value. The returned write-back
// moves the box content (possibly replaced by the native code, e.g. the
// error matched by errors.As) into the interpreted variable cell; for
// aliased pointers it is a semantic no-op.
func genPtrAliasInterfaceWrapper(n *node, typ reflect.Type) (func(*frame) reflect.Value, func(*frame, reflect.Value)) {
	inner := genInterfaceWrapper(n, typ)
	cell := genValue(n.child[0])
	return func(f *frame) reflect.Value {
			w := inner(f)
			if w.IsValid() && w.Kind() == reflect.Struct && isInterfaceWrapperType(w.Type()) && w.CanAddr() {
				return w.Addr()
			}
			return w
		}, func(f *frame, passed reflect.Value) {
			box := passed
			for box.IsValid() && box.Kind() == reflect.Ptr {
				if box.IsNil() {
					return
				}
				box = box.Elem()
			}
			if !box.IsValid() || box.Kind() != reflect.Struct || !isInterfaceWrapperType(box.Type()) {
				return
			}
			iv := box.Field(0)
			if !iv.IsValid() || !iv.CanInterface() {
				return
			}
			cv := iv.Interface()
			if cv == nil {
				return
			}
			var value reflect.Value
			if vi, ok := cv.(valueInterface); ok {
				value = vi.value
			} else {
				value = reflect.ValueOf(cv)
			}
			if !value.IsValid() {
				return
			}
			dest := cell(f)
			if !dest.IsValid() || !dest.CanSet() || dest.Type() != value.Type() || dest == value {
				return
			}
			f.interp.markOwnedCellWriteFromExec(dest, value)
			dest.Set(value)
		}
}
