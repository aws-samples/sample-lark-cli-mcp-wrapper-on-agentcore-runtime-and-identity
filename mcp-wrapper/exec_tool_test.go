package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeBinary creates a tiny shell script standing in for lark-cli, so
// this test exercises runLarkExec's argument-passing and output-capturing
// logic without needing a real lark-cli build or Feishu credentials.
func writeFakeBinary(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-lark-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0755); err != nil {
		t.Fatalf("failed to write fake binary: %v", err)
	}
	return path
}

func TestRunLarkExec_CapturesStdout(t *testing.T) {
	bin := writeFakeBinary(t, `echo "arg1=$1 arg2=$2"`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkExec(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	want := "arg1=hello arg2=world\n"
	if result.Stdout != want {
		t.Errorf("stdout = %q, want %q", result.Stdout, want)
	}
}

func TestRunLarkExec_CapturesNonZeroExit(t *testing.T) {
	bin := writeFakeBinary(t, `echo "boom" >&2; exit 3`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkExec(context.Background(), "anything")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", result.ExitCode)
	}
	if result.Stderr != "boom\n" {
		t.Errorf("stderr = %q, want %q", result.Stderr, "boom\n")
	}
}

// On AgentCore Runtime the wrapper's own working directory is "/", which the
// non-root container user cannot write, so any lark-cli command that saves a
// file (--output is relative) failed with "permission denied". lark_exec
// therefore runs lark-cli in the temp directory.
func TestRunLarkExec_RunsInTheTempDirectory(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	bin := writeFakeBinary(t, `pwd -P`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkExec(context.Background(), "anything")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != want {
		t.Errorf("lark-cli working directory = %q, want %q", got, want)
	}
}

func TestRunLarkExec_RejectsEmptyCommand(t *testing.T) {
	_, err := runLarkExec(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty command, got nil")
	}
}

func TestRunLarkExec_DoubleQuotedArgumentKeepsSpaces(t *testing.T) {
	bin := writeFakeBinary(t, `echo "arg1=$1 arg2=$2 argc=$#"`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkExec(context.Background(), `--text "hello world" --flag`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "arg1=--text arg2=hello world argc=3\n"
	if result.Stdout != want {
		t.Errorf("stdout = %q, want %q", result.Stdout, want)
	}
}

func TestRunLarkExec_SingleQuotedArgumentKeepsSpaces(t *testing.T) {
	bin := writeFakeBinary(t, `echo "arg1=$1 argc=$#"`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkExec(context.Background(), `'hello world'`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "arg1=hello world argc=1\n"
	if result.Stdout != want {
		t.Errorf("stdout = %q, want %q", result.Stdout, want)
	}
}

func TestRunLarkExec_RejectsUnterminatedQuote(t *testing.T) {
	_, err := runLarkExec(context.Background(), `--text "unterminated`)
	if err == nil {
		t.Fatal("expected error for unterminated quote, got nil")
	}
}

// The lark-cli binary inherits this process's credential-bearing environment,
// so resolveLarkCLIBin refuses any path another local user could swap out from
// under it, rather than trusting LARK_CLI_BIN_PATH as-is.
func TestResolveLarkCLIBin_RejectsUnsafePaths(t *testing.T) {
	worldWritable := writeFakeBinary(t, `echo hi`)
	if err := os.Chmod(worldWritable, 0777); err != nil {
		t.Fatalf("failed to chmod fake binary: %v", err)
	}

	tests := []struct {
		name string
		path string
	}{
		{"relative path", "lark-cli"},
		{"missing file", filepath.Join(t.TempDir(), "does-not-exist")},
		{"directory", t.TempDir()},
		{"world-writable binary", worldWritable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LARK_CLI_BIN_PATH", tc.path)
			if _, err := resolveLarkCLIBin(); err == nil {
				t.Errorf("expected error for %s (%q), got nil", tc.name, tc.path)
			}
		})
	}
}

func TestResolveLarkCLIBin_AcceptsSafeBinary(t *testing.T) {
	bin := writeFakeBinary(t, `echo hi`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	got, err := resolveLarkCLIBin()
	if err != nil {
		t.Fatalf("unexpected error for safe binary %q: %v", bin, err)
	}
	if got != bin {
		t.Errorf("resolved path = %q, want %q", got, bin)
	}
}

// A sticky-bit directory (/tmp is 1777) is allowed even though it is
// world-writable: the sticky bit means only the owner can replace an existing
// entry. This keeps the documented local-dev flow that builds lark-cli into
// /tmp working.
func TestResolveLarkCLIBin_AllowsStickyDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0777|os.ModeSticky); err != nil {
		t.Fatalf("failed to chmod dir: %v", err)
	}
	path := filepath.Join(dir, "lark-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatalf("failed to write fake binary: %v", err)
	}
	t.Setenv("LARK_CLI_BIN_PATH", path)

	if _, err := resolveLarkCLIBin(); err != nil {
		t.Errorf("unexpected error for binary in sticky dir: %v", err)
	}
}

// The default must not point into a world-writable directory, since a planted
// binary there would run with the wrapper's full environment.
func TestLarkCLIBinPath_DefaultIsNotUnderTmp(t *testing.T) {
	t.Setenv("LARK_CLI_BIN_PATH", "")

	got := larkCLIBinPath()
	if got != defaultLarkCLIBin {
		t.Errorf("default = %q, want %q", got, defaultLarkCLIBin)
	}
	if strings.HasPrefix(got, "/tmp/") {
		t.Errorf("default %q is under /tmp, which is world-writable", got)
	}
}
