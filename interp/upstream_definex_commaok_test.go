package interp

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/traefik/yaegi/stdlib"
)

// These tests check comma-ok definitions of multiple variables by a single
// value at package (REPL root) scope: type assertions, map indexes and
// channel receives, in short var decl and var spec forms. They used to fail
// with a "nil type" panic in gta, because compDefineX read the type of the
// right hand side before cfg computed it.

const commaOkNilMap = "var nilMapOk map[string]int"

func evalCommaOk(t *testing.T, src string) (reflect.Value, error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Eval panicked on %q: %v", src, r)
		}
	}()
	i := New(Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	return i.Eval(src)
}

// evalCommaOkPre evaluates pre in the interpreter first, then src, so a
// definition can be separated from its use across two Evals.
func evalCommaOkPre(t *testing.T, pre, src string) (reflect.Value, error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Eval panicked on %q: %v", src, r)
		}
	}()
	i := New(Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Eval(pre); err != nil {
		t.Fatalf("pre failed: %v", err)
	}
	return i.Eval(src)
}

func wantCommaOkValue(t *testing.T, src, want string) {
	t.Helper()
	v, err := evalCommaOk(t, src)
	if err != nil {
		t.Fatalf("got error %v, want none", err)
	}
	if got := fmt.Sprint(v); got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func wantCommaOkError(t *testing.T, src, want string) {
	t.Helper()
	_, err := evalCommaOk(t, src)
	if err == nil {
		t.Fatalf("got no error, want %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want error containing %q", err, want)
	}
}

func TestRootDefineXTypeAssert(t *testing.T) {
	wantCommaOkValue(t, `j := interface{}(3)
_, ok := j.(int)
ok`, "true")

	wantCommaOkValue(t, `j2 := interface{}(3)
v, ok := j2.(int)
[]interface{}{v, ok}`, "[3 true]")

	wantCommaOkValue(t, `j3 := interface{}("s")
v, ok := j3.(int)
[]interface{}{v, ok}`, "[0 false]")

	wantCommaOkValue(t, `j4 := interface{}(1.5)
_, ok := j4.(int)
ok`, "false")

	// The single-value (short) form must keep working at root.
	wantCommaOkValue(t, `j5 := interface{}(3)
v := j5.(int)
v`, "3")

	// Inside a function (cfg path) must keep working.
	wantCommaOkValue(t, `func f() bool {
	i := interface{}(3)
	_, ok := i.(int)
	return ok
}
f()`, "true")
}

func TestRootDefineXMapIndex(t *testing.T) {
	wantCommaOkValue(t, `m := map[string]int{"a": 1}
v, ok := m["a"]
[]interface{}{v, ok}`, "[1 true]")

	wantCommaOkValue(t, `m2 := map[string]int{"a": 1}
v, ok := m2["z"]
[]interface{}{v, ok}`, "[0 false]")

	wantCommaOkValue(t, `m3 := map[string]int{"a": 1}
_, ok := m3["a"]
ok`, "true")

	// Reading a nil map is like reading an empty map.
	wantCommaOkValue(t, commaOkNilMap+`
v, ok := nilMapOk["a"]
[]interface{}{v, ok}`, "[0 false]")

	wantCommaOkValue(t, `func g() bool {
	m := map[string]int{"a": 1}
	_, ok := m["a"]
	return ok
}
g()`, "true")
}

func TestRootDefineXChanRecv(t *testing.T) {
	wantCommaOkValue(t, `c := make(chan int, 1)
c <- 7
v, ok := <-c
[]interface{}{v, ok}`, "[7 true]")

	wantCommaOkValue(t, `c2 := make(chan int, 1)
c2 <- 7
_, ok := <-c2
ok`, "true")

	// A closed channel yields the zero value and false.
	wantCommaOkValue(t, `c3 := make(chan int, 1)
close(c3)
v, ok := <-c3
[]interface{}{v, ok}`, "[0 false]")
}

func TestRootDefineXVarSpecForm(t *testing.T) {
	wantCommaOkValue(t, `l := interface{}(4)
var vv, oo = l.(int)
[]interface{}{vv, oo}`, "[4 true]")

	wantCommaOkValue(t, `m4 := map[string]int{"a": 1}
var mv, mo = m4["a"]
[]interface{}{mv, mo}`, "[1 true]")

	wantCommaOkValue(t, `c4 := make(chan int, 1)
c4 <- 9
var cv, co = <-c4
[]interface{}{cv, co}`, "[9 true]")
}

func TestRootDefineXForwardType(t *testing.T) {
	// The type is declared after its use in the same source: gta must
	// revisit the definition once the type is known.
	wantCommaOkValue(t, `p := interface{}(5)
var pv, po = p.(T)
type T int
[]interface{}{pv, po}`, "[5 true]")

	wantCommaOkValue(t, `q := interface{}(6)
qv, qo := q.(U)
type U int
[]interface{}{qv, qo}`, "[6 true]")
}

func TestRootDefineXCrossEval(t *testing.T) {
	v, err := evalCommaOkPre(t, `m5 := map[string]int{"k": 5}`, `v, ok := m5["k"]
[]interface{}{v, ok}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(v); got != "[5 true]" {
		t.Fatalf("got %v, want [5 true]", got)
	}

	v, err = evalCommaOkPre(t, `j6 := interface{}(42)`, `v6, ok6 := j6.(int)
[]interface{}{v6, ok6}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(v); got != "[42 true]" {
		t.Fatalf("got %v, want [42 true]", got)
	}
}

func TestRootDefineXNativeSources(t *testing.T) {
	i := New(Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	ch := make(chan int, 1)
	ch <- 9
	if err := i.Use(Exports{
		"testpkg/testpkg": {
			"M": reflect.ValueOf(map[string]int{"a": 1}),
			// A function returning interface{} provides a value whose static
			// type is an interface, unlike reflect.ValueOf(interface{}(3))
			// which yields the dynamic type.
			"F":  reflect.ValueOf(func() interface{} { var v interface{} = 3; return v }),
			"Ch": reflect.ValueOf(ch),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Eval(`import "testpkg"`); err != nil {
		t.Fatal(err)
	}

	v, err := i.Eval(`v, ok := testpkg.M["a"]
[]interface{}{v, ok}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(v); got != "[1 true]" {
		t.Fatalf("got %v, want [1 true]", got)
	}

	v, err = i.Eval(`v2, ok2 := testpkg.M["z"]
[]interface{}{v2, ok2}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(v); got != "[0 false]" {
		t.Fatalf("got %v, want [0 false]", got)
	}

	v, err = i.Eval(`v3, ok3 := testpkg.F().(int)
[]interface{}{v3, ok3}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(v); got != "[3 true]" {
		t.Fatalf("got %v, want [3 true]", got)
	}

	v, err = i.Eval(`v4, ok4 := <-testpkg.Ch
[]interface{}{v4, ok4}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(v); got != "[9 true]" {
		t.Fatalf("got %v, want [9 true]", got)
	}
}

func TestRootDefineXGlobalReadFromCallChain(t *testing.T) {
	// Symbols defined by a root comma-ok must be global: reads from nested
	// interpreted function calls resolve the root frame.
	wantCommaOkValue(t, `zz := interface{}(42)
zv, zo := zz.(int)
func h() []interface{} { return []interface{}{zv, zo} }
func hh() []interface{} { return h() }
hh()`, "[42 true]")
}

func TestRootDefineXErrors(t *testing.T) {
	// Wrong number of destinations.
	wantCommaOkError(t, `r := interface{}(5)
r1, r2, r3 := r.(int)`, "assignment mismatch: 3 variables")

	// Comma-ok is only valid for a map index.
	wantCommaOkError(t, `s := "ab"
s1, s2 := s[0]`, "assignment mismatch: 2 variables")

	// Receiving from a non-channel.
	wantCommaOkError(t, `xx := 5
xx1, xx2 := <-xx`, "cannot receive from non-channel")

	// Undefined source.
	wantCommaOkError(t, `_, ok := nodemap["k"]`, "undefined: nodemap")

	// Undefined asserted type, resolvable in no later pass.
	wantCommaOkError(t, `q := interface{}(5)
var qv, qo = q.(Missing)`, "undefined type: main.Missing")
}

func TestRootDefineXRedeclare(t *testing.T) {
	wantCommaOkValue(t, `w := 1
w, wok := interface{}(3).(int)
[]interface{}{w, wok}`, "[3 true]")

	// A declaration in an inner block must not shadow the outer one.
	wantCommaOkValue(t, `v7 := 1
if true {
	v8, ok7 := interface{}(3).(int)
	_, _ = v8, ok7
}
v7`, "1")
}

func TestRootDefineXParenDerefMap(t *testing.T) {
	wantCommaOkValue(t, `m6 := map[string]int{"a": 1}
p := &m6
v, ok := (*p)["a"]
[]interface{}{v, ok}`, "[1 true]")
}
