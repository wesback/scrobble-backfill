// Package harness holds a placeholder verifying this module's Go toolchain
// and test runner work end to end. Delete this file once a real package
// exists elsewhere in the module for `go test ./...` to run instead.
package harness

import "testing"

func TestHarness(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("arithmetic is broken")
	}
}
