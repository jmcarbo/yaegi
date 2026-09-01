package main

// Comma-ok definitions of multiple variables by a single value at package
// scope, including types declared after their use in the file.

var i interface{} = 3

var _, okA = i.(int)

var s, okB = fs().(S)

var w, okC = fw().(Q)

func fs() interface{} { var v interface{} = S{7}; return v }

func fw() interface{} { var v interface{} = Q{5}; return v }

func main() {
	m := map[string]int{"a": 1}
	_, okD := m["a"]
	c := make(chan int, 1)
	c <- 7
	r, okE := <-c
	println(okA, okB, okC, okD, okE, s.N, w.N, r)
}

type S struct{ N int }

type Q struct{ N int }

// Output:
// true true true true true 7 5 7
