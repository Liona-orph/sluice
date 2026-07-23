// Package leaktest detects goroutines that outlive the test that started them.
//
// The streaming paths are the reason it exists. An iterator-based stream is
// supposed to leave nothing running when a consumer abandons it, and "supposed
// to" is not a property a reviewer can check by reading; a test that abandons a
// stream mid-flight and then asserts the goroutine count returned to its
// starting value is.
//
// It counts goroutines rather than reading their stacks. That is cruder than
// the well-known third-party leak detectors -- it cannot name the offender --
// but it needs no dependency and no parsing of an unspecified format, and for a
// test that either leaks one goroutine or leaks none the count is enough.
package leaktest

import (
	"runtime"
	"testing"
	"time"
)

// Check registers a check that runs when the test finishes.
//
//	func TestStream(t *testing.T) {
//	    defer leaktest.Check(t)()
//	    ...
//	}
//
// Or, more simply, call it and ignore the returned function: it also registers
// itself with t.Cleanup. The returned function is provided for the defer form,
// which reads better when the check is the point of the test.
func Check(t *testing.T) func() {
	t.Helper()
	before := stable()
	check := func() {
		t.Helper()
		// Goroutines wind down asynchronously, so a single sample produces
		// flaky failures. Poll until the count comes back or the budget runs
		// out; a real leak never comes back and pays the full budget once.
		deadline := time.Now().Add(2 * time.Second)
		for {
			after := runtime.NumGoroutine()
			if after <= before {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("goroutine leak: %d before, %d after", before, after)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Cleanup(check)
	// The cleanup already runs the check; the returned closure is a no-op so
	// that the defer form does not double-report.
	return func() {}
}

// stable samples the goroutine count after giving the runtime a chance to
// settle, so that goroutines left over from an earlier test are not attributed
// to this one.
func stable() int {
	prev := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		time.Sleep(time.Millisecond)
		n := runtime.NumGoroutine()
		if n == prev {
			return n
		}
		prev = n
	}
	return prev
}
