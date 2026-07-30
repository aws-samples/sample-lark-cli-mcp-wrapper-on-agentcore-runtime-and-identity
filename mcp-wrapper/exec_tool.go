package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// defaultLarkCLIBin is where the container image installs lark-cli (see
// deploy/agentcore-runtime/entrypoint.sh). Deliberately not a path under /tmp:
// the binary named here inherits this process's credential-bearing
// environment, so the default must not live in a world-writable directory.
const defaultLarkCLIBin = "/usr/local/bin/lark-cli"

// larkCLIBinPath returns the configured path to the lark-cli binary this
// wrapper shells out to. Configurable via LARK_CLI_BIN_PATH so the same
// wrapper binary can point at different lark-cli builds without recompiling.
// Callers should go through resolveLarkCLIBin, which validates the result.
func larkCLIBinPath() string {
	if p := os.Getenv("LARK_CLI_BIN_PATH"); p != "" {
		return p
	}
	return defaultLarkCLIBin
}

// resolveLarkCLIBin validates the configured lark-cli path before it is handed
// to exec. The path is deployment configuration rather than MCP caller input,
// but it decides which binary runs with this process's environment (including
// LARKSUITE_CLI_AUTH_PROXY and anything else the sidecar exports), so it is
// checked rather than trusted: it must be absolute, an existing regular file
// (not a symlink, directory, or device), and neither it nor its directory may
// be writable by group or other. The sticky-bit exemption on the directory
// check is what allows the documented local-dev flow that builds lark-cli into
// /tmp: /tmp is 1777, and with the sticky bit set only the owner can replace an
// existing entry, whereas a world-writable directory without it lets any local
// process swap the binary out between builds.
func resolveLarkCLIBin() (string, error) {
	p := larkCLIBinPath()
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("lark-cli path must be absolute, got %q", p)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return "", fmt.Errorf("lark-cli not usable at %q: %w", p, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("lark-cli at %q is not a regular file (mode %s)", p, fi.Mode())
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("lark-cli at %q is group/world-writable (mode %04o)", p, fi.Mode().Perm())
	}
	dirPath := filepath.Dir(p)
	dir, err := os.Stat(dirPath)
	if err != nil {
		return "", fmt.Errorf("lark-cli directory %q not usable: %w", dirPath, err)
	}
	if dir.Mode().Perm()&0o022 != 0 && dir.Mode()&os.ModeSticky == 0 {
		return "", fmt.Errorf("lark-cli directory %q is group/world-writable without the sticky bit (mode %04o)", dirPath, dir.Mode().Perm())
	}
	return p, nil
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

	bin, err := resolveLarkCLIBin()
	if err != nil {
		return execResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// The command path is validated by resolveLarkCLIBin above, and args is
	// passed as argv rather than through a shell (no "sh -c"), so shell
	// metacharacters (;, |, `, &&) are not interpreted -- they reach lark-cli
	// as literal argument text. Exposing lark-cli's own command surface is the
	// intended contract of lark_exec; authorization belongs at the
	// Gateway/IAM layer, not here.
	// nosemgrep: dangerous-exec-command
	cmd := exec.CommandContext(ctx, bin, args...)
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
