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
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"ghealth/pkg/auth"
	"ghealth/pkg/config"
)

const testClientSecret = `{"installed":{"client_id":"cid.apps.googleusercontent.com","client_secret":"csecret","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token"}}`

func testCredentials(refresh string) string {
	creds := `{"access_token":"at","token_type":"Bearer","expiry":"` +
		time.Now().Add(time.Hour).Format(time.RFC3339) + `"`
	if refresh != "" {
		creds += `,"refresh_token":"` + refresh + `"`
	}
	return creds + "}"
}

// isolateConfigDir points the config package at a scratch directory so a test
// never reads or writes the developer's real credentials.
func isolateConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GHEALTH_CONFIG_DIR", dir)
	t.Setenv(EnvClientSecret, "")
	t.Setenv(EnvCredentials, "")
	return dir
}

func TestBootstrapIsANoOpWithoutEnvironmentCredentials(t *testing.T) {
	isolateConfigDir(t)
	written, err := BootstrapCredentials()
	if err != nil {
		t.Fatalf("BootstrapCredentials: %v", err)
	}
	if len(written) != 0 {
		t.Errorf("wrote %v with nothing configured", written)
	}
}

func TestBootstrapWritesRawJSON(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv(EnvClientSecret, testClientSecret)
	t.Setenv(EnvCredentials, testCredentials("rt"))

	written, err := BootstrapCredentials()
	if err != nil {
		t.Fatalf("BootstrapCredentials: %v", err)
	}
	if len(written) != 2 {
		t.Fatalf("wrote %v, want both files", written)
	}
	assertOwnerOnlyJSON(t, config.ClientSecretPath())
	assertOwnerOnlyJSON(t, config.CredentialsPath())

	// The CLI must be able to load what the bootstrap wrote.
	creds, err := auth.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds.RefreshToken != "rt" {
		t.Errorf("refresh token %q, want rt", creds.RefreshToken)
	}
}

// Hosting dashboards mangle multi-line values, so base64 has to work too.
func TestBootstrapAcceptsBase64(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv(EnvClientSecret, base64.StdEncoding.EncodeToString([]byte(testClientSecret)))
	t.Setenv(EnvCredentials, base64.StdEncoding.EncodeToString([]byte(testCredentials("rt"))))

	if _, err := BootstrapCredentials(); err != nil {
		t.Fatalf("BootstrapCredentials: %v", err)
	}
	if _, err := auth.LoadCredentials(); err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
}

// A refresh token cannot be redeemed without the client credentials that
// issued it, so the mistake is reported at startup rather than as a puzzling
// auth error on the first tool call.
func TestBootstrapRejectsARefreshTokenWithoutItsClientSecret(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv(EnvCredentials, testCredentials("rt"))

	_, err := BootstrapCredentials()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), EnvClientSecret) {
		t.Errorf("error should name the missing variable: %v", err)
	}
	if _, statErr := os.Stat(config.CredentialsPath()); statErr == nil {
		t.Error("credentials were written despite the error")
	}
}

// An access-token-only import has no refresh path and so needs no client
// secret; it should be accepted.
func TestBootstrapAcceptsAccessTokenOnlyCredentials(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv(EnvCredentials, testCredentials(""))

	written, err := BootstrapCredentials()
	if err != nil {
		t.Fatalf("BootstrapCredentials: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("wrote %v, want just the credentials file", written)
	}
}

// The CLI rotates refresh tokens and persists the new one. A stale environment
// variable must never overwrite that.
func TestBootstrapNeverOverwritesExistingFiles(t *testing.T) {
	dir := isolateConfigDir(t)
	if err := os.WriteFile(dir+"/credentials.json", []byte(testCredentials("rotated")), 0600); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	t.Setenv(EnvClientSecret, testClientSecret)
	t.Setenv(EnvCredentials, testCredentials("stale"))

	written, err := BootstrapCredentials()
	if err != nil {
		t.Fatalf("BootstrapCredentials: %v", err)
	}
	if len(written) != 1 || !strings.HasSuffix(written[0], "client_secret.json") {
		t.Errorf("wrote %v, want only the client secret", written)
	}

	creds, err := auth.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds.RefreshToken != "rotated" {
		t.Errorf("refresh token %q — the stale environment value clobbered the stored one", creds.RefreshToken)
	}
}

func TestBootstrapRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name        string
		secret      string
		credentials string
		wantSub     string
	}{
		{"client secret is not JSON", "{not json", "", "not valid JSON"},
		{"client secret is neither JSON nor base64", "%%%%", "", "neither JSON nor valid base64"},
		{"credentials are not JSON", testClientSecret, "{nope", "not valid JSON"},
		{"credentials carry no token", testClientSecret, `{"token_type":"Bearer"}`, "access_token or refresh_token"},
		{"base64 decodes to something that is not JSON", base64.StdEncoding.EncodeToString([]byte("hello")), "", "not valid JSON"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigDir(t)
			t.Setenv(EnvClientSecret, tc.secret)
			t.Setenv(EnvCredentials, tc.credentials)

			_, err := BootstrapCredentials()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

func assertOwnerOnlyJSON(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("%s has mode %o, want 600 — these are credentials", path, perm)
	}
}

// Under Google sign-in every caller brings their own credentials, so the
// operator's must not be installed — a copy of their refresh token on disk
// would be a secret stored for nothing. The command decides this; this test
// pins the behaviour BootstrapCredentials must have when it *is* called, namely
// that it is the only thing that writes them.
func TestBootstrapIsTheOnlyWriterOfOperatorCredentials(t *testing.T) {
	dir := isolateConfigDir(t)
	t.Setenv(EnvClientSecret, testClientSecret)
	t.Setenv(EnvCredentials, testCredentials("rt"))

	// Not called: nothing should exist yet.
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("config dir is not empty before the bootstrap runs: %v (%v)", entries, err)
	}

	if _, err := BootstrapCredentials(); err != nil {
		t.Fatalf("BootstrapCredentials: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected both credential files after the bootstrap, got %v (%v)", entries, err)
	}
}
