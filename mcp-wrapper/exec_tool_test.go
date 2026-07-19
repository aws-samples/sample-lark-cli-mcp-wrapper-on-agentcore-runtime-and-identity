package main

import (
	"context"
	"os"
	"path/filepath"
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
