package interp_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/traefik/yaegi/interp"
)

// TestGorneshCanceledSourceImportRetryEvaluatesCleanly pins the drained
// cancel-retry sequence for a source package: cancel an import whose
// initializer blocks in native code, let the canceled worker fully drain, and
// then evaluate the same import again. Before the detached-root sanitize fix,
// the retry committed the import but the first evaluation of the expression
// panicked with "reflect: call of reflect.Value.SetString on func Value"
// because recompiled slots reused stale typed cells from the canceled run.
func TestGorneshCanceledSourceImportRetryEvaluatesCleanly(t *testing.T) {
	var calls atomic.Int32
	blocked := make(chan struct{})
	release := make(chan struct{})
	i := interp.New(interp.Options{
		GoPath: "gopath",
		SourcecodeFilesystem: fstest.MapFS{
			"gopath/src/cncl/cncl.go": {Data: []byte(`package cncl
import "cnclhost"
var A = cnclhost.Make("a")
var B = cnclhost.Make("b")
`)},
		},
	})
	makeFn := func(s string) string {
		if calls.Add(1) == 1 {
			close(blocked)
			<-release
		}
		return s
	}
	if err := i.Use(interp.Exports{"cnclhost/cnclhost": {"Make": reflect.ValueOf(makeFn)}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		_, err := i.EvalWithContext(ctx, `import "cncl"; cncl.A + cncl.B`)
		res <- err
	}()
	<-blocked
	cancel()
	if err := <-res; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled import err = %v, want context.Canceled", err)
	}
	close(release)
	// Let the canceled worker drain so the retry takes the failed-root
	// detach path; retrying while the zombie is still parked does not.
	time.Sleep(100 * time.Millisecond)

	v, err := i.Eval(`import "cncl"; cncl.A + cncl.B`)
	if err != nil {
		t.Fatalf("retry eval failed: %v", err)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("init host calls = %d, want at least 2", got)
	}
	if got, ok := v.Interface().(string); !ok || got != "ab" {
		t.Fatalf("retry value = %v (%T), want ab", v, v.Interface())
	}

	// Repeated evaluations stay stable after the recovered retry.
	if _, err := i.Eval(`import "cncl"; cncl.A + cncl.B`); err != nil {
		t.Fatalf("follow-up eval failed: %v", err)
	}
}

// TestGorneshOwnedAllocationScanStaysBounded guards the O(1) fast paths in
// the per-write ownership scans: an interpreter that retains a few thousand
// owned allocations must not pay a registry walk per instruction. The bound
// is deliberately generous; before the fast paths this workload took about a
// minute, afterwards it runs in tens of milliseconds.
func TestGorneshOwnedAllocationScanStaysBounded(t *testing.T) {
	i := interp.New(interp.Options{})
	if err := i.Use(interp.Exports{"gorneshfix/gorneshfix": {"Noop": reflect.ValueOf(func(v int) int { return v })}}); err != nil {
		t.Fatal(err)
	}
	i.ImportUsed()
	src := `
keep := make([][]int, 0, 2000)
for i := 0; i < 2000; i++ {
	keep = append(keep, make([]int, 4))
}
s := 0
for i := 0; i < 100000; i++ { s += i }
gorneshfix.Noop(s)
`
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := i.Eval(src)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("owned-allocation scan did not stay bounded: eval still running after 10s (elapsed %v)", time.Since(start))
	}
}

// TestGorneshRetainedCallbackStillWorksAfterBoundedSweeps exercises the
// call-path memoization: a host-retained closure passed repeatedly to a
// native function must keep working across many invocations and later Evals.
func TestGorneshRetainedCallbackStillWorksAfterBoundedSweeps(t *testing.T) {
	i := interp.New(interp.Options{})
	total := 0
	if err := i.Use(interp.Exports{"gorneshfix/gorneshfix": {
		"Apply": reflect.ValueOf(func(f func(int) int) int { total = f(total); return total }),
	}}); err != nil {
		t.Fatal(err)
	}
	i.ImportUsed()
	if _, err := i.Eval(`
f := func(x int) int { return x + 3 }
for i := 0; i < 500; i++ {
	gorneshfix.Apply(f)
}
`); err != nil {
		t.Fatal(err)
	}
	if total != 1500 {
		t.Fatalf("total = %d, want 1500", total)
	}
	// The same closure keeps working in a later Eval.
	if _, err := i.Eval(`gorneshfix.Apply(f)`); err != nil {
		t.Fatal(err)
	}
	if total != 1503 {
		t.Fatalf("total after later Eval = %d, want 1503", total)
	}
}
