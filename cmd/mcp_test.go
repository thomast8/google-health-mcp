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

package cmd

import (
	"strings"
	"testing"

	"ghealth/pkg/mcpauth"
)

func TestPublicURL(t *testing.T) {
	tests := []struct {
		name     string
		explicit string
		railway  string
		want     string
	}{
		{"explicit wins", "https://health.example.com", "ignored.up.railway.app", "https://health.example.com"},
		{"trailing slash trimmed", "https://health.example.com/", "", "https://health.example.com"},
		// Railway injects the domain without a scheme, so the server completes
		// it — which is why GHEALTH_MCP_PUBLIC_URL is optional there.
		{"railway domain gains a scheme", "", "ghealth.up.railway.app", "https://ghealth.up.railway.app"},
		{"neither set", "", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envPublicURL, tc.explicit)
			t.Setenv(envRailwayDomain, tc.railway)
			if got := publicURL(); got != tc.want {
				t.Errorf("publicURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The suggested secret has to be long enough for the provider to accept, or the
// operator pastes it in and hits the same error again.
func TestSuggestedSecretIsUsable(t *testing.T) {
	steps := suggestSecretSteps(envMCPSecret)
	if len(steps) < 4 {
		t.Fatalf("no generated value offered: %v", steps)
	}

	last := steps[len(steps)-1]
	idx := strings.LastIndex(last, ": ")
	if idx < 0 {
		t.Fatalf("cannot find the value in %q", last)
	}
	secret := last[idx+2:]

	if len(secret) < mcpauth.MinSecretLength {
		t.Errorf("suggested secret is %d chars, but the provider requires %d",
			len(secret), mcpauth.MinSecretLength)
	}
	if _, err := mcpauth.NewProvider(mcpauth.Config{
		Secret: secret,
		Google: mcpauth.GoogleConfig{ClientID: "id", ClientSecret: "secret"},
	}); err != nil {
		t.Errorf("the suggested secret is rejected by the provider: %v", err)
	}
}

func TestSuggestedSecretsAreUnique(t *testing.T) {
	first := suggestSecretSteps(envMCPSecret)
	second := suggestSecretSteps(envMCPSecret)
	if first[len(first)-1] == second[len(second)-1] {
		t.Error("the same secret was suggested twice — it must be freshly generated")
	}
}

func TestMCPTimeout(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv(envMCPTimeout, "")
		d, err := mcpTimeout()
		if err != nil {
			t.Fatalf("mcpTimeout: %v", err)
		}
		if d <= 0 {
			t.Errorf("timeout %v", d)
		}
	})
	for _, bad := range []string{"soon", "0s", "-5s", "60"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			t.Setenv(envMCPTimeout, bad)
			if _, err := mcpTimeout(); err == nil {
				t.Errorf("%q was accepted", bad)
			}
		})
	}
	t.Run("accepts a duration", func(t *testing.T) {
		t.Setenv(envMCPTimeout, "45s")
		d, err := mcpTimeout()
		if err != nil {
			t.Fatalf("mcpTimeout: %v", err)
		}
		if d.Seconds() != 45 {
			t.Errorf("timeout %v, want 45s", d)
		}
	})
}

// A half-configured Google client must be reported, never silently downgraded to
// the shared token — that would expose the operator's own record to anyone
// holding it.
func TestHTTPAuthOptionsRejectsPartialGoogleConfig(t *testing.T) {
	t.Setenv(envGoogleClientID, "id.apps.googleusercontent.com")
	t.Setenv(envGoogleClientSecret, "")
	t.Setenv(envMCPSecret, "")
	t.Setenv(envMCPToken, "a-shared-token")

	opts, err := httpAuthOptions(func(string, ...any) {})
	if err == nil {
		t.Fatalf("partial config was accepted: %+v", opts)
	}
	if opts.Token != "" || opts.OAuth != nil {
		t.Error("a partial config produced usable options")
	}
	if !strings.Contains(err.Error(), envGoogleClientSecret) {
		t.Errorf("the error does not name what is missing: %v", err)
	}
}

func TestHTTPAuthOptionsSelectsMode(t *testing.T) {
	t.Run("google sign-in when fully configured", func(t *testing.T) {
		t.Setenv(envGoogleClientID, "id.apps.googleusercontent.com")
		t.Setenv(envGoogleClientSecret, "client-secret")
		t.Setenv(envMCPSecret, "0123456789abcdef0123456789abcdef")
		t.Setenv(envMCPToken, "also-set")
		t.Setenv(envPublicURL, "https://example.com")

		opts, err := httpAuthOptions(func(string, ...any) {})
		if err != nil {
			t.Fatalf("httpAuthOptions: %v", err)
		}
		if opts.OAuth == nil {
			t.Fatal("Google sign-in was not selected")
		}
		// Google sign-in must win, so a leftover token cannot expose the
		// operator's account once multi-user mode is on.
		if opts.Token != "" {
			t.Error("the shared token was also returned")
		}
	})

	t.Run("shared token when only it is set", func(t *testing.T) {
		t.Setenv(envGoogleClientID, "")
		t.Setenv(envGoogleClientSecret, "")
		t.Setenv(envMCPSecret, "")
		t.Setenv(envMCPToken, "a-shared-token")

		opts, err := httpAuthOptions(func(string, ...any) {})
		if err != nil {
			t.Fatalf("httpAuthOptions: %v", err)
		}
		if opts.OAuth != nil || opts.Token != "a-shared-token" {
			t.Errorf("unexpected options: %+v", opts)
		}
	})

	t.Run("nothing configured leaves it to the handler", func(t *testing.T) {
		for _, name := range []string{envGoogleClientID, envGoogleClientSecret, envMCPSecret, envMCPToken} {
			t.Setenv(name, "")
		}
		opts, err := httpAuthOptions(func(string, ...any) {})
		if err != nil {
			t.Fatalf("httpAuthOptions: %v", err)
		}
		if opts.OAuth != nil || opts.Token != "" {
			t.Errorf("unexpected options: %+v", opts)
		}
	})
}

// Google rejects an authorization request outright if one scope in it is
// unavailable to the project, so trimming the list has to be possible from
// configuration alone.
func TestGoogleScopes(t *testing.T) {
	t.Run("unset means the defaults", func(t *testing.T) {
		t.Setenv(envGoogleScopes, "")
		got, err := googleScopes()
		if err != nil {
			t.Fatalf("googleScopes: %v", err)
		}
		if got != nil {
			t.Errorf("got %v, want nil so the provider defaults apply", got)
		}
	})

	t.Run("bare suffixes are expanded", func(t *testing.T) {
		t.Setenv(envGoogleScopes, "sleep.readonly, activity_and_fitness.readonly")
		got, err := googleScopes()
		if err != nil {
			t.Fatalf("googleScopes: %v", err)
		}
		want := []string{
			"openid", "email",
			"https://www.googleapis.com/auth/googlehealth.sleep.readonly",
			"https://www.googleapis.com/auth/googlehealth.activity_and_fitness.readonly",
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("got %v\nwant %v", got, want)
		}
	})

	t.Run("full URLs pass through and duplicates collapse", func(t *testing.T) {
		full := "https://www.googleapis.com/auth/googlehealth.sleep.readonly"
		t.Setenv(envGoogleScopes, full+" sleep.readonly")
		got, err := googleScopes()
		if err != nil {
			t.Fatalf("googleScopes: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("got %v, want openid, email and one health scope", got)
		}
	})

	// The account address identifies a session in auth_status, so these two are
	// not the caller's to drop.
	t.Run("openid and email are always present", func(t *testing.T) {
		t.Setenv(envGoogleScopes, "sleep.readonly")
		got, err := googleScopes()
		if err != nil {
			t.Fatalf("googleScopes: %v", err)
		}
		if got[0] != "openid" || got[1] != "email" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("a list with no health scopes is an error", func(t *testing.T) {
		t.Setenv(envGoogleScopes, "openid, email, ,")
		if _, err := googleScopes(); err == nil {
			t.Error("an empty health scope list was accepted")
		}
	})
}
