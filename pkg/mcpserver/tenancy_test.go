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
	"strings"
	"testing"

	"ghealth/pkg/mcpauth"
)

// sessionCtx attaches a resolver that yields the given Google token, standing
// in for an authenticated request.
func sessionCtx(token string) context.Context {
	return mcpauth.WithSession(context.Background(), &mcpauth.Session{Email: "user@example.com"},
		func(context.Context) (string, error) { return token, nil })
}

// printEnv runs the shell and echoes one environment variable, so a test can
// see exactly what a child process was handed.
func printEnv(t *testing.T, r *ExecRunner, ctx context.Context, name string) (string, error) {
	t.Helper()
	r.Binary = shellBinary(t)
	out, err := r.Run(ctx, "-c", `printf '%s' "${`+name+`-<unset>}"`)
	return string(out), err
}

// The core multi-tenant guarantee: no session, no data. A request that reaches
// the runner unauthenticated must fail rather than fall back to whatever
// credentials the server process itself holds.
func TestPerRequestAuthRefusesWithoutASession(t *testing.T) {
	r := &ExecRunner{PerRequestAuth: true, Binary: shellBinary(t)}

	out, err := r.Run(context.Background(), "-c", "echo should-not-run")
	if err == nil {
		t.Fatalf("an unauthenticated call succeeded and produced %q", out)
	}
	if !strings.Contains(err.Error(), "not connected to a Google account") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPerRequestAuthInjectsTheCallersToken(t *testing.T) {
	r := &ExecRunner{PerRequestAuth: true}
	got, err := printEnv(t, r, sessionCtx("alice-google-token"), "GHEALTH_ACCESS_TOKEN")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "alice-google-token" {
		t.Errorf("child saw GHEALTH_ACCESS_TOKEN=%q, want the caller's token", got)
	}
}

// Two callers in flight must never see each other's credential.
func TestPerRequestAuthKeepsCallersApart(t *testing.T) {
	r := &ExecRunner{PerRequestAuth: true}

	alice, err := printEnv(t, r, sessionCtx("alice-token"), "GHEALTH_ACCESS_TOKEN")
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	bob, err := printEnv(t, r, sessionCtx("bob-token"), "GHEALTH_ACCESS_TOKEN")
	if err != nil {
		t.Fatalf("bob: %v", err)
	}

	if alice != "alice-token" || bob != "bob-token" {
		t.Fatalf("tokens crossed: alice saw %q, bob saw %q", alice, bob)
	}
}

// The CLI resolves credentials from several sources in precedence order. Under
// per-request auth every one of them is stripped, so there is exactly one
// credential in the child's environment and no argument about precedence.
func TestPerRequestAuthStripsEveryOtherCredentialSource(t *testing.T) {
	stripped := []string{
		"GHEALTH_CREDENTIALS_FILE",
		"GHEALTH_PROFILE",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GHEALTH_CLIENT_SECRET_JSON",
		"GHEALTH_CREDENTIALS_JSON",
		"GHEALTH_MCP_TOKEN",
		"GHEALTH_MCP_SECRET",
		"GHEALTH_MCP_GOOGLE_CLIENT_ID",
		"GHEALTH_MCP_GOOGLE_CLIENT_SECRET",
	}
	for _, name := range stripped {
		t.Setenv(name, "leaked-value")
	}

	r := &ExecRunner{PerRequestAuth: true, ConfigDir: t.TempDir()}
	for _, name := range stripped {
		got, err := printEnv(t, r, sessionCtx("tok"), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != "<unset>" {
			t.Errorf("child inherited %s=%q — it must not be able to authenticate as anyone else", name, got)
		}
	}
}

// The config directory is the last place stored credentials could come from,
// so children are pointed at an empty one.
func TestPerRequestAuthIsolatesTheConfigDir(t *testing.T) {
	t.Setenv("GHEALTH_CONFIG_DIR", "/home/operator/.config/ghealth")
	empty := t.TempDir()

	r := &ExecRunner{PerRequestAuth: true, ConfigDir: empty}
	got, err := printEnv(t, r, sessionCtx("tok"), "GHEALTH_CONFIG_DIR")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != empty {
		t.Errorf("child config dir %q, want the isolated %q", got, empty)
	}
}

// Single-account mode is unchanged: the CLI's own credential resolution is
// exactly what is wanted, so the environment passes through.
func TestSingleAccountModeInheritsTheEnvironment(t *testing.T) {
	t.Setenv("GHEALTH_CREDENTIALS_FILE", "/creds.json")

	r := &ExecRunner{}
	got, err := printEnv(t, r, context.Background(), "GHEALTH_CREDENTIALS_FILE")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "/creds.json" {
		t.Errorf("child saw %q, want the inherited value", got)
	}
}

func TestStripEnv(t *testing.T) {
	in := []string{"KEEP=1", "DROP=2", "ALSO_KEEP=3", "MALFORMED"}
	got := stripEnv(in, []string{"DROP"})

	want := map[string]bool{"KEEP=1": true, "ALSO_KEEP=3": true, "MALFORMED": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, kv := range got {
		if !want[kv] {
			t.Errorf("unexpected entry %q", kv)
		}
	}
}

// auth_status is how a user confirms whose data they are looking at, which
// matters most when a deployment serves several people.
func TestAuthStatusNamesTheConnectedAccount(t *testing.T) {
	r := &recordingRunner{stdout: []byte(`{"authenticated":true,"method":"env token"}`)}

	res, _, err := authStatus(r)(sessionCtx("tok"), nil, authStatusInput{})
	if err != nil {
		t.Fatalf("authStatus: %v", err)
	}
	got := resultText(t, res)
	if !strings.Contains(got, "user@example.com") {
		t.Errorf("the result does not name the connected account:\n%s", got)
	}
	if !strings.Contains(got, `"authenticated": true`) {
		t.Errorf("the CLI's own fields were lost:\n%s", got)
	}
}

// Cosmetic enrichment must never be able to break the tool.
func TestAuthStatusPassesThroughUnparseableOutput(t *testing.T) {
	r := &recordingRunner{stdout: []byte("not json at all")}

	res, _, err := authStatus(r)(sessionCtx("tok"), nil, authStatusInput{})
	if err != nil {
		t.Fatalf("authStatus: %v", err)
	}
	if got := resultText(t, res); got != "not json at all" {
		t.Errorf("output was altered: %q", got)
	}
}

// Without a session there is no account to name, and the CLI's output stands
// on its own.
func TestAuthStatusIsUnchangedWithoutASession(t *testing.T) {
	payload := `{"authenticated":true}`
	r := &recordingRunner{stdout: []byte(payload)}

	res, _, err := authStatus(r)(context.Background(), nil, authStatusInput{})
	if err != nil {
		t.Fatalf("authStatus: %v", err)
	}
	if got := resultText(t, res); got != payload {
		t.Errorf("output %q, want it unchanged", got)
	}
}
