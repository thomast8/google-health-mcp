// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package mcpserver exposes the ghealth CLI as a Model Context Protocol server.
//
// Every tool is a thin mapping from MCP arguments onto a ghealth command line.
// The CLI is invoked as a child process rather than called in-process, which
// keeps one behaviour to maintain instead of two: filter construction,
// pagination, rollup range caps, response simplification, the _hints layer and
// the structured error envelope all stay in cmd/ and pkg/output, and an MCP
// tool returns byte-for-byte what the equivalent CLI command prints. It is also
// what makes concurrent tool calls safe — the cobra tree keeps its flag state in
// package-level variables and writes to os.Stdout, neither of which survives
// being shared between simultaneous requests.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"ghealth/pkg/client"
)

// DefaultTimeout bounds one CLI invocation. List operations auto-paginate, so
// this is generous relative to a single API round trip.
const DefaultTimeout = 120 * time.Second

// killGrace is how long Run waits for a timed-out child's output pipes to close
// after it has been killed, before giving up on them.
const killGrace = 2 * time.Second

// Runner executes a ghealth command line and returns whatever it wrote to
// stdout. A non-zero exit becomes an error carrying the CLI's own message,
// hint and recovery steps.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// ExecRunner runs the ghealth binary as a child process.
type ExecRunner struct {
	// Binary is the ghealth executable to invoke. Empty means "this process's
	// own executable", which is the normal case: the MCP server is a subcommand
	// of the same binary it shells out to.
	Binary string
	// Timeout bounds a single invocation. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Run executes `ghealth <args...>` and returns its stdout.
//
// args are passed to the child process directly, never through a shell, so
// argument values cannot inject additional commands or flags beyond the
// position they occupy.
func (r *ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin := r.Binary
	if bin == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("cannot locate the ghealth executable: %w", err)
		}
		bin = self
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Killing the child on timeout does not close the output pipe if it left a
	// descendant holding the write end; without a WaitDelay, Run would block
	// until that descendant exited and the deadline would mean nothing.
	cmd.WaitDelay = killGrace
	err := cmd.Run()

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("ghealth %s timed out after %s — narrow the range with from/to, or lower limit",
			strings.Join(args, " "), timeout)
	case errors.Is(ctx.Err(), context.Canceled):
		// The caller gave up (client disconnected, request cancelled). Report
		// that rather than the child's "signal: killed", which reads like a bug.
		return nil, fmt.Errorf("ghealth %s was cancelled", strings.Join(args, " "))
	}
	if err != nil {
		return nil, cliFailure(stderr.Bytes(), err)
	}
	return stdout.Bytes(), nil
}

// cliFailure turns a non-zero exit into an error an agent can act on. The CLI
// writes {"error": {...}} to stderr for every failure it recognises; that
// envelope carries the message, a hint and sometimes a recovery checklist,
// all of which are more useful to the caller than an exit status.
func cliFailure(stderr []byte, runErr error) error {
	var envelope struct {
		Error *client.CLIError `json:"error"`
	}
	trimmed := bytes.TrimSpace(stderr)
	if len(trimmed) > 0 && json.Unmarshal(trimmed, &envelope) == nil && envelope.Error != nil {
		return errors.New(formatCLIError(envelope.Error))
	}

	// Unstructured failure (a cobra usage error, or the binary dying before it
	// could write an envelope). Relay stderr so the cause is not swallowed.
	if len(trimmed) > 0 {
		return fmt.Errorf("ghealth failed: %s", trimmed)
	}
	return fmt.Errorf("ghealth failed: %w", runErr)
}

// formatCLIError renders a CLI error envelope as readable text, preserving the
// hint and next-step checklist the CLI supplies for recoverable failures.
func formatCLIError(e *client.CLIError) string {
	var b strings.Builder
	if e.Type != "" {
		fmt.Fprintf(&b, "%s error: ", e.Type)
	}
	b.WriteString(e.Message)
	if e.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.Status)
	}
	if e.Hint != "" {
		fmt.Fprintf(&b, "\nHint: %s", e.Hint)
	}
	if len(e.NextSteps) > 0 {
		b.WriteString("\nNext steps:")
		for _, step := range e.NextSteps {
			fmt.Fprintf(&b, "\n  - %s", step)
		}
	}
	return b.String()
}
