package main

import (
	"fmt"
	"reflect"
)

type f struct{ X int }

func (fb *f) One() { fmt.Println("1") }

func main() {
	var fb f
	reflect.ValueOf(&fb).MethodByName("One").Call([]reflect.Value{})
}

// Output:
// 1
