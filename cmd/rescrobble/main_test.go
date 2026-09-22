package main

import (
	"bytes"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if got := run([]string{"--version"}, &stdout, &stderr); got != 0 {
		t.Fatalf("run returned exit code %d, want 0", got)
	}
	if got := stdout.String(); got != version+"\n" {
		t.Fatalf("version output = %q, want %q", got, version+"\n")
	}
	if stderr.Len() != 0 {
		t.Fatalf("version wrote stderr: %q", stderr.String())
	}
}
