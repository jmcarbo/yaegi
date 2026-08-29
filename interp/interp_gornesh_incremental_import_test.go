package interp_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

func TestGorneshIncrementalBinaryImportRemainsUsable(t *testing.T) {
	i := newGorneshImportInterpreter(t)

	evalGorneshImport(t, i, `import "fmt"`)
	if got := evalGorneshImport(t, i, `fmt.Sprintf("%d", 41)`).Interface(); got != "41" {
		t.Fatalf("first imported package use = %#v, want %q", got, "41")
	}
	evalGorneshImport(t, i, `import "fmt"`)
	if got := evalGorneshImport(t, i, `fmt.Sprintf("%d", 42)`).Interface(); got != "42" {
		t.Fatalf("repeated imported package use = %#v, want %q", got, "42")
	}
}

func TestGorneshSameFileDuplicateBinaryImportsFail(t *testing.T) {
	tests := map[string]string{
		"default": `import ("fmt"; "fmt")`,
		"aliases": `import (first "fmt"; second "fmt")`,
		"dot":     `import (. "fmt"; . "fmt")`,
		"blank":   `import (_ "fmt"; _ "fmt")`,
	}
	for name, src := range tests {
		t.Run(name, func(t *testing.T) {
			i := newGorneshImportInterpreter(t)
			if _, err := i.Eval(src); err == nil || !strings.Contains(err.Error(), "redeclared") {
				t.Fatalf("duplicate import error = %v, want redeclaration diagnostic", err)
			}
		})
	}
}

func TestGorneshIncrementalBinaryImportBindings(t *testing.T) {
	t.Run("aliases", func(t *testing.T) {
		i := newGorneshImportInterpreter(t)
		evalGorneshImport(t, i, `import format "fmt"`)
		evalGorneshImport(t, i, `import format "fmt"`)
		evalGorneshImport(t, i, `import formatAgain "fmt"`)
		if _, err := i.Eval(`import format "strings"`); err == nil || !strings.Contains(err.Error(), "redeclared") {
			t.Fatalf("alias collision error = %v, want redeclaration diagnostic", err)
		}
		if got := evalGorneshImport(t, i, `format.Sprintf("%s", formatAgain.Sprintf("%s", "ok"))`).Interface(); got != "ok" {
			t.Fatalf("aliases after collision = %#v, want %q", got, "ok")
		}
	})

	t.Run("dot", func(t *testing.T) {
		i := newGorneshImportInterpreter(t)
		evalGorneshImport(t, i, `import . "strings"`)
		evalGorneshImport(t, i, `import . "strings"`)
		if got := evalGorneshImport(t, i, `ToUpper("go")`).Interface(); got != "GO" {
			t.Fatalf("dot import result = %#v, want %q", got, "GO")
		}
		if _, err := i.Eval(`import . "bytes"`); err == nil || !strings.Contains(err.Error(), "redeclared") {
			t.Fatalf("dot import collision error = %v, want redeclaration diagnostic", err)
		}
		if got := evalGorneshImport(t, i, `ToUpper("still usable")`).Interface(); got != "STILL USABLE" {
			t.Fatalf("dot import after collision = %#v, want %q", got, "STILL USABLE")
		}
	})

	t.Run("blank", func(t *testing.T) {
		i := newGorneshImportInterpreter(t)
		evalGorneshImport(t, i, `import _ "fmt"`)
		evalGorneshImport(t, i, `import _ "fmt"`)
		evalGorneshImport(t, i, `import (_ "bytes"; _ "strings")`)
	})
}

func TestGorneshBinaryImportCanRetryAfterFailedEval(t *testing.T) {
	i := newGorneshImportInterpreter(t)

	if _, err := i.Eval(`import "fmt"; var importRetryBroken = missingImportRetryName`); err == nil {
		t.Fatal("Eval with undefined initializer unexpectedly succeeded")
	}
	evalGorneshImport(t, i, `import "fmt"`)
	if got := evalGorneshImport(t, i, `fmt.Sprintf("retry-%d", 42)`).Interface(); got != "retry-42" {
		t.Fatalf("retried import result = %#v, want %q", got, "retry-42")
	}
}

func newGorneshImportInterpreter(t *testing.T) *interp.Interpreter {
	t.Helper()
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		t.Fatal(err)
	}
	return i
}

func evalGorneshImport(t *testing.T, i *interp.Interpreter, src string) reflect.Value {
	t.Helper()
	v, err := i.Eval(src)
	if err != nil {
		t.Fatalf("Eval(%q): %v", src, err)
	}
	return v
}
