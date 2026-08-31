package interp_test

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
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
type CanFly interface{ Fly() string }

type flyBridge struct{ mc interp.MethodCaller }

func (b flyBridge) Fly() string {
	r, err := b.mc.CallMethod("Fly", nil)
	if err != nil {
		return err.Error()
	}
	return r[0].String()
}

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

// errors.As with an error-interface target keeps working (regression guard).
func TestBridgeErrorsAsIfaceTarget(t *testing.T) {
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

func (t *T) Error() string { return "T" }

func run() {
	var e error
	w := fmt.Errorf("outer: %w", &T{V: 1})
	if !errors.As(w, &e) || e == nil {
		panic("interface target not matched")
	}
	native := fmt.Errorf("native: %w", errors.New("plain"))
	if !errors.As(native, &e) || e.Error() != "native: plain" {
		panic("native chain not matched")
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

// errors.As with a value-receiver error type as target.
func TestBridgeErrorsAsValueTarget(t *testing.T) {
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

type V struct{ N int }

func (v V) Error() string { return "V" }

func run() {
	var t V
	w := fmt.Errorf("outer: %w", V{N: 7})
	if !errors.As(w, &t) || t.N != 7 {
		panic("value target not matched")
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

// Type switches and assertions on values coming back from binary land
// recover the interpreted view (#681).
func TestBridgeTypeSwitchRoundTrip(t *testing.T) {
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

type P struct{ N int }

func (p *P) Error() string { return "P" }

func run() {
	w := fmt.Errorf("outer: %w", &P{N: 9})
	u := errors.Unwrap(w)
	switch x := u.(type) {
	case *P:
		if x.N != 9 {
			panic("wrong value in type switch")
		}
	default:
		panic("type switch did not match")
	}
	var i any = u
	if p, ok := i.(*P); !ok || p.N != 9 {
		panic("type assertion on any var did not match")
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

// json.Unmarshal into a nil map with interpreted element unmarshalers
// allocates the map, as native Go does.
func TestBridgeJSONNilMap(t *testing.T) {
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

type P struct{ N int }

func (p *P) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	p.N = n
	return nil
}

func run() {
	var m map[string]*P
	if err := json.Unmarshal([]byte("{\"a\": 1}"), &m); err != nil {
		panic(err)
	}
	if m == nil {
		panic("nil map unmarshal failed: m nil")
	}
	if m["a"] == nil {
		panic("nil map unmarshal failed: m[a] nil")
	}
	if m["a"].N != 1 {
		panic("nil map unmarshal failed: N")
	}
	fmt.Println("ok")
}
`
	if _, err := i.Eval(src); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Eval("main.run()"); err != nil {
		t.Fatal(err)
	}
}

// TextUnmarshaler leaves get the bare string, not the JSON-quoted form.
func TestBridgeJSONTextUnmarshaler(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	src := `
package main

import (
	"encoding"
	"encoding/json"
	"fmt"
)

type CT struct{ S string }

func (c *CT) UnmarshalText(b []byte) error {
	c.S = "TEXT<" + string(b) + ">"
	return nil
}

var _ encoding.TextUnmarshaler = (*CT)(nil)

func run() {
	var xs []CT
	if err := json.Unmarshal([]byte("[\"2026-08-31\"]"), &xs); err != nil {
		panic(err)
	}
	if len(xs) != 1 || xs[0].S != "TEXT<2026-08-31>" {
		panic(fmt.Sprintf("bad text unmarshal: %v", xs))
	}
	fmt.Println("ok")
}
`
	if _, err := i.Eval(src); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Eval("main.run()"); err != nil {
		t.Fatal(err)
	}
}

// Concurrent RegisterBridge and boxing must not race (the host-bridge
// snapshot is taken under the bridge lock).
func TestBridgeRegisterBridgeRace(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Eval(`package main

type bird struct{}

func (bird) Fly() string { return "flying" }

func Make() any { return bird{} }
`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for k := 0; k < 8; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			interp.RegisterBridge[CanFly](i, func(mc interp.MethodCaller) (CanFly, bool) {
				return flyBridge{mc}, true
			})
			v, err := i.Eval("main.Make()")
			if err != nil {
				t.Error(err)
				return
			}
			fly, ok := v.Interface().(CanFly)
			if !ok {
				t.Errorf("k=%d: not CanFly: %T", k, v.Interface())
				return
			}
			if got := fly.Fly(); got != "flying" {
				t.Errorf("k=%d: %q", k, got)
			}
		}(k)
	}
	wg.Wait()
}
