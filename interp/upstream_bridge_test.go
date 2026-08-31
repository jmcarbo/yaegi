package interp_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// The reflect.Value method introspection family surfaces interpreted methods
// on unwrapped values (upstream #847).
func TestBridgeReflectMethodFamily(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	src := `
package main

import "reflect"

type base struct{ B int }

func (b base) GetB() int { return b.B }

type T struct {
	base
	A int
}

func (t *T) SetA(v int) { t.A = v }

var gt = T{base: base{B: 2}, A: 1}
`
	if _, err := i.Eval(src); err != nil {
		t.Fatal(err)
	}
	runTests(t, i, []testCase{
		{src: "reflect.ValueOf(&main.gt).NumMethod()", res: "2"}, // GetB (promoted) + SetA
		{src: "reflect.ValueOf(&main.gt).MethodByName(\"SetA\").Type().String()", res: "func(int)"},
		{src: "reflect.ValueOf(&main.gt).MethodByName(\"GetB\").Call(nil)[0].Int()", res: "2"},
		{src: "reflect.ValueOf(&main.gt).MethodByName(\"SetA\").Call([]reflect.Value{reflect.ValueOf(3)}); main.gt.A", res: "3"},
		{src: "reflect.ValueOf(&main.gt).MethodByName(\"Missing\").IsValid()", res: "false"},
		{src: "reflect.ValueOf(&main.gt).MethodByName(\"hidden\").IsValid()", res: "false"},
		{src: "reflect.ValueOf(main.gt).MethodByName(\"SetA\").IsValid()", res: "false"}, // pointer receiver hidden on value
		{src: "reflect.ValueOf(main.gt).MethodByName(\"GetB\").Call(nil)[0].Int()", res: "2"},
		{src: "reflect.ValueOf(&main.gt).Type().NumMethod()", res: "0"}, // documented asymmetry
	})
}

// A bridged method value keeps working when it crosses back and forth through
// host interfaces.
func TestBridgeReflectMethodFuncRoundTrip(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	src := `
package main

import "reflect"

type C struct{ N int }

func (c *C) Inc(by int) { c.N += by }

var counter = C{N: 1}

func methodValue() reflect.Value {
	return reflect.ValueOf(&counter).MethodByName("Inc")
}

func counterValue() int { return counter.N }
`
	if _, err := i.Eval(src); err != nil {
		t.Fatal(err)
	}
	v, err := i.Eval("main.methodValue")
	if err != nil {
		t.Fatal(err)
	}
	f, ok := v.Interface().(func() reflect.Value)
	if !ok {
		t.Fatalf("methodValue: dynamic type %T", v.Interface())
	}
	mv := f()
	if got := mv.Type().String(); got != "func(int)" {
		t.Fatalf("method value type: %s", got)
	}
	fnv, ok := mv.Interface().(func(int))
	if !ok {
		t.Fatalf("method value interface: %T", mv.Interface())
	}
	fnv(41)
	res, err := i.Eval("main.counterValue()")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Interface(); got != 42 {
		t.Fatalf("counter: %v", got)
	}
}

// errors.As over interpreted errors: positive, negative and native chains.
func TestBridgeErrorsAsMatrix(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	src := `
package main

import (
	"errors"
	"fmt"
)

type T struct{ V int }

func (t *T) Error() string { return fmt.Sprintf("T(%d)", t.V) }

type U struct{}

func (u *U) Error() string { return "U" }

func findT(err error) bool {
	var t *T
	return errors.As(err, &t)
}

func main() {
	wrapped := fmt.Errorf("outer: %w", fmt.Errorf("mid: %w", &T{V: 3}))
	if !findT(wrapped) {
		panic("T not found through two wraps")
	}
	if findT(fmt.Errorf("no T here: %w", &U{})) {
		panic("T found in a U chain")
	}
	if findT(nil) {
		panic("T found in nil")
	}
	native := fmt.Errorf("native: %w", errors.New("plain"))
	if findT(native) {
		panic("T found in a native chain")
	}
	var t2 *T
	wrapped2 := fmt.Errorf("w: %w", &T{V: 4})
	if !errors.As(wrapped2, &t2) || t2.V != 4 {
		panic("target V not preserved")
	}
	if errors.Is(wrapped2, &T{V: 5}) {
		panic("Is matched a different value")
	}
	if !errors.Unwrap(wrapped2).Error().(string)[0:1].(string)[:0] + "" == "" {
	}
}
`
	// The last check above is deliberately avoided; simplify the source.
	src = strings.Replace(src, `	if !errors.Unwrap(wrapped2).Error().(string)[0:1].(string)[:0] + "" == "" {
	}
`, "", 1)
	if _, err := i.Eval(src); err != nil {
		t.Fatal(err)
	}
}

// Eval-boundary re-boxing: interpreted values satisfy native interfaces and
// the dynamic type is a native box (never interp.valueInterface).
func TestBridgeEvalBoundaryStringer(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	src := `
package main

type Key struct{ ID string }

func (k Key) String() string { return "key:" + k.ID }

func makeAny() any { return Key{ID: "k1"} }
`
	if _, err := i.Eval(src); err != nil {
		t.Fatal(err)
	}
	v, err := i.Eval("main.makeAny()")
	if err != nil {
		t.Fatal(err)
	}
	hosted := v.Interface()
	s, ok := hosted.(fmt.Stringer)
	if !ok {
		t.Fatalf("hosted %T does not implement fmt.Stringer", hosted)
	}
	if got := s.String(); got != "key:k1" {
		t.Fatalf("String: %q", got)
	}
	// Pin the documented identity limitation: the box is an anonymous or
	// interpreter-owned native type, not the interpreted struct.
	if n := reflect.TypeOf(hosted).Name(); n != "" && !strings.HasPrefix(n, "_") {
		t.Fatalf("unexpected box type %v", reflect.TypeOf(hosted))
	}
}

type CanEat interface{ Eat() }

type canEatBridge struct{ mc interp.MethodCaller }

func (b canEatBridge) Eat() {
	if _, err := b.mc.CallMethod("Eat", nil); err != nil {
		panic(err)
	}
}

// RegisterBridge lets host code satisfy a host-declared interface with an
// interpreted value (#939).
func TestBridgeRegisterBridge(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	interp.RegisterBridge[CanEat](i, func(mc interp.MethodCaller) (CanEat, bool) {
		return canEatBridge{mc}, true
	})
	src := `
package main

import "fmt"

type man struct{ n string }

func (m man) Eat() { fmt.Println("I'm eating", m.n) }

type CanEat interface{ Eat() }

var M CanEat = man{n: "bob"}

func makeMan() any { return man{n: "alice"} }
`
	if _, err := i.Eval(src); err != nil {
		t.Fatal(err)
	}
	m, err := i.Eval("main.M")
	if err != nil {
		t.Fatal(err)
	}
	man, ok := m.Interface().(CanEat)
	if !ok {
		t.Fatalf("host assertion failed: %T", m.Interface())
	}
	man.Eat()
	v, err := i.Eval("main.makeMan()")
	if err != nil {
		t.Fatal(err)
	}
	man2, ok := v.Interface().(CanEat)
	if !ok {
		t.Fatalf("any result assertion failed: %T", v.Interface())
	}
	man2.Eat()
}

// Per-element marshaler dispatch through containers (#1486).
func TestBridgeJSONContainerShapes(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	src := `
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Obj struct {
	Name    string
	Surname string
}

type custom struct {
	FullName string
}

func (o Obj) MarshalJSON() ([]byte, error) {
	return json.Marshal(&custom{FullName: o.Name + " " + o.Surname})
}

func (o *Obj) UnmarshalJSON(b []byte) error {
	var c custom
	if err := json.Unmarshal(b, &c); err != nil {
		return err
	}
	fields := strings.Fields(c.FullName)
	if len(fields) != 2 {
		return fmt.Errorf("bad name: %q", c.FullName)
	}
	o.Name, o.Surname = fields[0], fields[1]
	return nil
}

func run() {
	objs := []Obj{{"A", "B"}, {"C", "D"}}
	b, err := json.Marshal(objs)
	if err != nil {
		panic(err)
	}
	want := "[{\"FullName\":\"A B\"},{\"FullName\":\"C D\"}]"
	if string(b) != want {
		panic("marshal: " + string(b))
	}
	var back []Obj
	if err := json.Unmarshal(b, &back); err != nil {
		panic(err)
	}
	if len(back) != 2 || back[0].Name != "A" || back[1].Surname != "D" {
		panic(fmt.Sprintf("unmarshal: %v", back))
	}
	m := map[string]Obj{"x": {"X", "Y"}}
	b2, _ := json.Marshal(m)
	if string(b2) != "{\"x\":{\"FullName\":\"X Y\"}}" {
		panic("map marshal: " + string(b2))
	}
	pts := []*Obj{{"P", "Q"}}
	b3, _ := json.Marshal(pts)
	if string(b3) != "[{\"FullName\":\"P Q\"}]" {
		panic("ptr slice marshal: " + string(b3))
	}
}
`
	if _, err := i.Eval(src); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Eval("main.run()"); err != nil {
		t.Fatal(err)
	}
}

// Invalid recursive types are rejected at compile time instead of crashing
// reflect (#1534).
func TestBridgeInvalidRecursiveType(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	_, err := i.Eval(`package main

type A struct {
	S string
	a A
}

func main() {
	var x A
	_ = x.S
}
`)
	if err == nil || !strings.Contains(err.Error(), "invalid recursive type") {
		t.Fatalf("expected invalid recursive type error, got: %v", err)
	}
}
