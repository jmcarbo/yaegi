package main

import (
	"errors"
	"fmt"
)

type T struct{}

func (t *T) Error() string { return "T error" }

func main() {
	var target *T
	wrapped := fmt.Errorf("wrap: %w", &T{})
	ok := errors.As(wrapped, &target)
	fmt.Println("found:", ok, target != nil)
}
