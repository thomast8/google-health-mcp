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

package mcpserver

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A recoverable CLI failure carries a hint and sometimes a checklist. Both have
// to survive the trip to the client, or the agent cannot act on them.
func TestCLIFailurePreservesHintAndNextSteps(t *testing.T) {
	stderr := []byte(`{
  "error": {
    "type": "config",
    "code": 5,
    "message": "No OAuth client_secret.json configured",
    "hint": "Run 'ghealth setup' to create or import OAuth credentials",
    "next_steps": [
      "Open https://console.cloud.google.com/apis/credentials",
      "Create OAuth client ID with Application type: Desktop app"
    ]
  }
}`)

	err := cliFailure(stderr, errors.New("exit status 5"))
	msg := err.Error()

	for _, want := range []string{
		"config error",
		"No OAuth client_secret.json configured",
		"Hint: Run 'ghealth setup'",
		"Next steps:",
		"- Open https://console.cloud.google.com/apis/credentials",
		"- Create OAuth client ID with Application type: Desktop app",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q:\n%s", want, msg)
		}
	}
}

func TestCLIFailureIncludesTheHTTPStatus(t *testing.T) {
	stderr := []byte(`{"error":{"type":"api","code":1,"status":403,"message":"insufficient authentication scopes"}}`)
	msg := cliFailure(stderr, errors.New("exit status 1")).Error()
	if !strings.Contains(msg, "HTTP 403") {
		t.Errorf("message is missing the status:\n%s", msg)
	}
}

// Not every failure produces an envelope — a cobra usage error does not. That
// output still has to reach the caller rather than being replaced by an
// exit code.
func TestCLIFailureRelaysUnstructuredStderr(t *testing.T) {
	msg := cliFailure([]byte("Error: unknown flag: --nope\n"), errors.New("exit status 1")).Error()
	if !strings.Contains(msg, "unknown flag: --nope") {
		t.Errorf("stderr was swallowed:\n%s", msg)
	}
}

func TestCLIFailureFallsBackToTheRunError(t *testing.T) {
	msg := cliFailure(nil, errors.New("signal: killed")).Error()
	if !strings.Contains(msg, "signal: killed") {
		t.Errorf("run error was swallowed:\n%s", msg)
	}
}

func TestExecRunnerReturnsStdout(t *testing.T) {
	r := &ExecRunner{Binary: shellBinary(t)}
	out, err := r.Run(context.Background(), "-c", `printf '{"dataPoints":[]}'`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(out) != `{"dataPoints":[]}` {
		t.Errorf("stdout %q", out)
	}
}

func TestExecRunnerDecodesAFailureEnvelope(t *testing.T) {
	r := &ExecRunner{Binary: shellBinary(t)}
	_, err := r.Run(context.Background(), "-c",
		`printf '{"error":{"type":"auth","code":2,"message":"not authenticated","hint":"Run ghealth auth login"}}' >&2; exit 2`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "auth error: not authenticated") || !strings.Contains(err.Error(), "Hint:") {
		t.Errorf("unexpected message: %v", err)
	}
}

// A slow call must surface as a timeout the caller can act on, not hang the
// MCP session until the client gives up.
func TestExecRunnerTimesOut(t *testing.T) {
	r := &ExecRunner{Binary: shellBinary(t), Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := r.Run(context.Background(), "-c", "sleep 10")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("unexpected message: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s to time out", elapsed)
	}
}

func shellBinary(t *testing.T) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh is unavailable: %v", err)
	}
	return sh
}
