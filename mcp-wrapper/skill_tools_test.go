package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeBinaryForSkills(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-lark-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0755); err != nil {
		t.Fatalf("failed to write fake binary: %v", err)
	}
	return path
}

func TestRunLarkListSkills_PassesThroughToExec(t *testing.T) {
	bin := writeFakeBinaryForSkills(t, `echo "got: $@"`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkListSkills(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "skills list") {
		t.Errorf("stdout %q does not contain expected command", result.Stdout)
	}
}

func TestRunLarkGetSkill_RejectsEmptyName(t *testing.T) {
	_, err := runLarkGetSkill(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty skillName, got nil")
	}
}

func TestRunLarkGetSkill_IncludesNameInCommand(t *testing.T) {
	bin := writeFakeBinaryForSkills(t, `echo "got: $@"`)
	t.Setenv("LARK_CLI_BIN_PATH", bin)

	result, err := runLarkGetSkill(context.Background(), "calendar-scheduling")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "calendar-scheduling") {
		t.Errorf("stdout %q does not contain skill name", result.Stdout)
	}
}
