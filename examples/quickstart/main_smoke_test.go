package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestQuickstartSmokeFailsWithoutEnv verifies the example exits non-zero
// (without panicking) when STRIPE_SECRET_KEY isn't set. This catches
// regressions like the slice-52 defer-after-Fatalf bug — if the defer
// chain is broken again, the exit message or behavior changes.
//
// We can't run the example with real keys in CI, but we can verify the
// no-env failure path is clean: exit code 1, stderr contains
// "STRIPE_SECRET_KEY is required", no panic.
//
// Build the binary into a temp file rather than `go run` so we don't
// pollute the test's $HOME with the Go module cache.
func TestQuickstartSmokeFailsWithoutEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "quickstart")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit; output: %s", out)
	}
	s := string(out)
	if !strings.Contains(s, "STRIPE_SECRET_KEY is required") {
		t.Fatalf("expected env-missing message; got: %s", s)
	}
	if strings.Contains(s, "panic:") {
		t.Fatalf("quickstart panicked instead of exiting cleanly: %s", s)
	}
}
