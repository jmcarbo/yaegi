package main

import (
	"encoding/json"
	"fmt"
)

type A struct{ B string }

func (a A) String() string { return "A<" + a.B + ">" }

func main() {
	// Marshalling of values held in a non-empty interface variable (#633).
	var s fmt.Stringer = A{B: "v"}
	j, err := json.Marshal(s)
	fmt.Println(string(j), err)

	var p fmt.Stringer = &A{B: "p"}
	jp, errp := json.Marshal(p)
	fmt.Println(string(jp), errp)

	// Slices and maps of interface-held values.
	sl := []fmt.Stringer{A{B: "1"}, &A{B: "2"}}
	jsl, errsl := json.Marshal(sl)
	fmt.Println(string(jsl), errsl)

	mp := map[string]fmt.Stringer{"k": A{B: "m"}}
	jmp, errmp := json.Marshal(mp)
	fmt.Println(string(jmp), errmp)

	// Unmarshal into an empty interface variable.
	var x interface{}
	erru := json.Unmarshal([]byte(`{"B":"u"}`), &x)
	fmt.Println(x, erru)

	// Method dispatch through the non-empty interface is unchanged.
	fmt.Println(s.String(), p.String())
}

// Output:
// {"B":"v"} <nil>
// {"B":"p"} <nil>
// [{"B":"1"},{"B":"2"}] <nil>
// {"k":{"B":"m"}} <nil>
// map[B:u] <nil>
// A<v> A<p>
