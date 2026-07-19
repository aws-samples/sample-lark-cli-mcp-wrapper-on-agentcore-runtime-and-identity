package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// larkCLIBinPath returns the path to the lark-cli binary this wrapper shells
// out to. Configurable via LARK_CLI_BIN_PATH so the same wrapper binary can
// point at different lark-cli builds without recompiling.
func larkCLIBinPath() string {
	if p := os.Getenv("LARK_CLI_BIN_PATH"); p != "" {
		return p
	}
	return "/tmp/lark-cli-sandbox"
}

// execResult is the structured outcome of running a lark-cli command,
// returned to the MCP caller as the tool result payload.
type execResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// splitCommandLine tokenizes a command line the way a shell would for the
// subset of syntax lark-cli commands need: whitespace separates words, and
// single or double quotes group a word that contains whitespace (e.g.
// --text "hello world"). Inside double quotes, a backslash escapes a
// following backslash or double-quote; single-quoted sections are fully
// literal (no escaping, matching POSIX shell single-quote semantics).
// Mismatched quotes return an error rather than silently truncating input,
// since a malformed --text value must not be silently misparsed.
func splitCommandLine(s string) ([]string, error) {
	var args []string
	var cur bytes.Buffer
	inWord := false
	i, n := 0, len(s)
	for i < n {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
			i++
		case c == '"':
			inWord = true
			i++
			for i < n && s[i] != '"' {
				if s[i] == '\\' && i+1 < n && (s[i+1] == '"' || s[i+1] == '\\') {
					cur.WriteByte(s[i+1])
					i += 2
					continue
				}
				cur.WriteByte(s[i])
				i++
			}
			if i >= n {
				return nil, fmt.Errorf("unterminated double quote in command line")
			}
			i++ // skip closing quote
		case c == '\'':
			inWord = true
			i++
			for i < n && s[i] != '\'' {
				cur.WriteByte(s[i])
				i++
			}
			if i >= n {
				return nil, fmt.Errorf("unterminated single quote in command line")
			}
			i++ // skip closing quote
		default:
			inWord = true
			cur.WriteByte(c)
			i++
		}
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}

// runLarkExec tokenizes argsLine with splitCommandLine (quote-aware, so
// arguments containing spaces — e.g. a --text value — must be wrapped in
// double or single quotes) and executes lark-cli with those arguments,
// inheriting the current process's environment (so sidecar env vars like
// LARKSUITE_CLI_AUTH_PROXY set on the wrapper process flow through to the
// lark-cli child process).
func runLarkExec(ctx context.Context, argsLine string) (execResult, error) {
	args, err := splitCommandLine(argsLine)
	if err != nil {
		return execResult{}, fmt.Errorf("invalid command line: %w", err)
	}
	if len(args) == 0 {
		return execResult{}, fmt.Errorf("empty command")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// nosemgrep: dangerous-exec-command -- args is passed as argv, not through
	// a shell (no "sh -c"), so shell metacharacters (;, |, `, &&) are not
	// interpreted; they reach lark-cli as literal argument text. This is the
	// intended contract of lark_exec: expose lark-cli's own command surface
	// to the MCP caller. Access control belongs at the Gateway/IAM layer,
	// not here.
	cmd := exec.CommandContext(ctx, larkCLIBinPath(), args...)
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	exitCode := 0
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	} else if runErr != nil {
		return execResult{}, fmt.Errorf("failed to run lark-cli: %w", runErr)
	}

	return execResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}
