// Copyright 2026 The xhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package testenv provides the subset of the Go project's internal test
// helpers needed by the relocated net/http tests.
package testenv

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Builder reports the Go builder name, or an empty string outside Go's build
// infrastructure.
func Builder() string {
	return os.Getenv("GO_BUILDER_NAME")
}

// MustHaveExec skips t on platforms that cannot start subprocesses.
func MustHaveExec(t testing.TB) {
	t.Helper()
	switch runtime.GOOS {
	case "ios", "js", "wasip1":
		t.Skipf("subprocess execution is unavailable on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

// Executable returns the path to the current test binary.
func Executable(t testing.TB) string {
	t.Helper()
	MustHaveExec(t)
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// CleanCmdEnv removes variables that can add unrelated diagnostic output to a
// subprocess launched by a test.
func CleanCmdEnv(cmd *exec.Cmd) *exec.Cmd {
	if cmd.Env != nil {
		panic("environment already set")
	}
	for _, env := range cmd.Environ() {
		if strings.HasPrefix(env, "GODEBUG=") || strings.HasPrefix(env, "GOTRACEBACK=") {
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	return cmd
}

// CommandContext returns an exec.Cmd after checking subprocess support.
func CommandContext(t testing.TB, ctx context.Context, name string, args ...string) *exec.Cmd {
	t.Helper()
	MustHaveExec(t)
	return exec.CommandContext(ctx, name, args...)
}

// Command returns an exec.Cmd after checking subprocess support.
func Command(t testing.TB, name string, args ...string) *exec.Cmd {
	t.Helper()
	return CommandContext(t, context.Background(), name, args...)
}

// GoTool reports the path to the Go command for the running toolchain.
func GoTool() (string, error) {
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if goroot := runtime.GOROOT(); goroot != "" {
		path := filepath.Join(goroot, "bin", name)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	path, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("locate Go tool: %w", err)
	}
	return path, nil
}

// GoToolPath returns the Go command path or fails t.
func GoToolPath(t testing.TB) string {
	t.Helper()
	path, err := GoTool()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// MustHaveGoRun skips t when the Go command cannot be found.
func MustHaveGoRun(t testing.TB) {
	t.Helper()
	MustHaveExec(t)
	if _, err := GoTool(); err != nil {
		t.Skipf("go run is unavailable: %v", err)
	}
}

// MustHaveSource skips t when the running toolchain has no Go source tree.
func MustHaveSource(t testing.TB) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(runtime.GOROOT(), "src", "net", "http")); err != nil {
		t.Skipf("Go source tree is unavailable: %v", err)
	}
}

var flaky = flag.Bool("flaky", false, "run tests known to be flaky in the upstream Go suite")

// SkipFlaky skips an upstream test unless -flaky was provided.
func SkipFlaky(t testing.TB, issue int) {
	t.Helper()
	if !*flaky {
		t.Skipf("skipping known flaky test; see go.dev/issue/%d", issue)
	}
}
