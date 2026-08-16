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

package mcpauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ghealth/pkg/auth"

	"golang.org/x/oauth2"
)

const testSecret = "0123456789abcdef0123456789abcdef"

// stubGoogle stands in for Google's OAuth endpoints.
type stubGoogle struct {
	*httptest.Server
	refreshCount int
	lastAuthReq  url.Values
	omitRefresh  bool
}

func newStubGoogle(t *testing.T) *stubGoogle {
	t.Helper()
	g := &stubGoogle{}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		g.lastAuthReq = r.URL.Query()
		// Google would show consent here; the stub redirects straight back.
		redirect, _ := url.Parse(r.URL.Query().Get("redirect_uri"))
		q := redirect.Query()
		q.Set("code", "google-code")
		q.Set("state", r.URL.Query().Get("state"))
		redirect.RawQuery = q.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		body := map[string]any{
			"access_token": "google-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		}
		if r.PostFormValue("grant_type") == "refresh_token" {
			g.refreshCount++
			body["access_token"] = "refreshed-access-token"
		} else if !g.omitRefresh {
			body["refresh_token"] = "google-refresh-token"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(body)
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

func (g *stubGoogle) endpoint() oauth2.Endpoint {
	return oauth2.Endpoint{AuthURL: g.URL + "/auth", TokenURL: g.URL + "/token"}
}

// harness is a provider fronted by a test HTTP server.
type harness struct {
	*httptest.Server
	provider *Provider
	google   *stubGoogle
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	g := newStubGoogle(t)

	h := &harness{google: g}
	provider, err := NewProvider(Config{
		Secret: testSecret,
		Google: GoogleConfig{
			ClientID:     "google-client-id",
			ClientSecret: "google-client-secret",
			Endpoint:     g.endpoint(),
			Scopes:       []string{"openid", "email"},
			TokenInfo: func(context.Context, string) (*auth.TokenInfo, error) {
				return &auth.TokenInfo{Email: "user@example.com"}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	h.provider = provider

	mux := http.NewServeMux()
	provider.Register(mux)
	h.Server = httptest.NewServer(mux)
	t.Cleanup(h.Close)

	// The provider derives its URLs from the request, and httptest serves over
	// plain HTTP on a loopback address; pin the origin so they resolve.
	provider.publicURL = h.URL
	return h
}

// noRedirectClient stops at each redirect so a test can inspect it.
func (h *harness) noRedirectClient() *http.Client {
	c := *h.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

const (
	testVerifier    = "a-code-verifier-of-sufficient-length-for-pkce"
	testRedirectURI = "https://chatgpt.com/connector_platform_oauth_redirect"
)

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (h *harness) register(t *testing.T, redirectURIs ...string) string {
	t.Helper()
	if len(redirectURIs) == 0 {
		redirectURIs = []string{testRedirectURI}
	}
	body, _ := json.Marshal(map[string]any{
		"redirect_uris": redirectURIs,
		"client_name":   "Test Client",
	})
	resp, err := h.Client().Post(h.URL+RegisterPath, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status %d", resp.StatusCode)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.ClientID == "" {
		t.Fatal("registration returned no client_id")
	}
	return out.ClientID
}

// authorize walks the browser half of the flow and returns the authorization
// code the client would receive.
func (h *harness) authorize(t *testing.T, clientID, challenge string) string {
	t.Helper()
	client := h.noRedirectClient()

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {testRedirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"client-state"},
	}
	resp, err := client.Get(h.URL + AuthorizePath + "?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize status %d, want the consent page", resp.StatusCode)
	}
	pending := extractPending(t, readBody(t, resp))

	// Approve, which federates to Google.
	form := url.Values{"pending": {pending}, "approve": {"yes"}}
	resp, err = client.PostForm(h.URL+ConsentPath, form)
	if err != nil {
		t.Fatalf("consent: %v", err)
	}
	defer resp.Body.Close()
	googleURL := resp.Header.Get("Location")
	if !strings.HasPrefix(googleURL, h.google.URL) {
		t.Fatalf("consent did not redirect to Google, got %q", googleURL)
	}

	// Google redirects back to our callback.
	resp, err = client.Get(googleURL)
	if err != nil {
		t.Fatalf("google auth: %v", err)
	}
	defer resp.Body.Close()
	callbackURL := resp.Header.Get("Location")

	resp, err = client.Get(callbackURL)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer resp.Body.Close()

	final, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("callback redirect: %v", err)
	}
	if got := final.Query().Get("state"); got != "client-state" {
		t.Errorf("state round-trip failed: got %q", got)
	}
	code := final.Query().Get("code")
	if code == "" {
		t.Fatalf("callback returned no code: %s", final)
	}
	return code
}

func (h *harness) exchange(t *testing.T, form url.Values) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := h.Client().PostForm(h.URL+TokenPath, form)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 1<<16)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}

func extractPending(t *testing.T, html string) string {
	t.Helper()
	const marker = `name="pending" value="`
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatalf("consent page has no pending field:\n%s", html)
	}
	rest := html[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatal("consent page pending field is unterminated")
	}
	return rest[:j]
}

// ─── discovery ───────────────────────────────────────────────────

// Without these documents an MCP client cannot find the authorization server,
// and the connector simply fails to add.
func TestDiscoveryMetadata(t *testing.T) {
	h := newHarness(t)

	t.Run("protected resource", func(t *testing.T) {
		for _, path := range []string{ProtectedResourcePath, ProtectedResourcePath + "/mcp"} {
			resp, err := h.Client().Get(h.URL + path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			defer resp.Body.Close()
			var doc struct {
				Resource             string   `json:"resource"`
				AuthorizationServers []string `json:"authorization_servers"`
			}
			json.NewDecoder(resp.Body).Decode(&doc)
			if doc.Resource != h.URL+"/mcp" {
				t.Errorf("%s: resource %q, want %q", path, doc.Resource, h.URL+"/mcp")
			}
			if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != h.URL {
				t.Errorf("%s: authorization_servers %v", path, doc.AuthorizationServers)
			}
		}
	})

	t.Run("authorization server", func(t *testing.T) {
		resp, err := h.Client().Get(h.URL + MetadataPath)
		if err != nil {
			t.Fatalf("metadata: %v", err)
		}
		defer resp.Body.Close()
		var doc struct {
			Issuer                string   `json:"issuer"`
			AuthorizationEndpoint string   `json:"authorization_endpoint"`
			TokenEndpoint         string   `json:"token_endpoint"`
			RegistrationEndpoint  string   `json:"registration_endpoint"`
			ChallengeMethods      []string `json:"code_challenge_methods_supported"`
			GrantTypes            []string `json:"grant_types_supported"`
		}
		json.NewDecoder(resp.Body).Decode(&doc)

		if doc.Issuer != h.URL {
			t.Errorf("issuer %q", doc.Issuer)
		}
		if doc.AuthorizationEndpoint != h.URL+AuthorizePath || doc.TokenEndpoint != h.URL+TokenPath {
			t.Errorf("endpoints: %+v", doc)
		}
		// ChatGPT and Claude both self-register; without this endpoint the
		// connector cannot be added at all.
		if doc.RegistrationEndpoint != h.URL+RegisterPath {
			t.Errorf("registration_endpoint %q", doc.RegistrationEndpoint)
		}
		if len(doc.ChallengeMethods) != 1 || doc.ChallengeMethods[0] != "S256" {
			t.Errorf("code_challenge_methods_supported %v, want [S256] only", doc.ChallengeMethods)
		}
		if !slicesContain(doc.GrantTypes, "refresh_token") {
			t.Errorf("grant_types_supported %v is missing refresh_token", doc.GrantTypes)
		}
	})
}

// ─── registration ────────────────────────────────────────────────

func TestRegistrationRejectsUnsafeRedirectURIs(t *testing.T) {
	h := newHarness(t)

	for _, tc := range []struct{ name, uri string }{
		{"plain http", "http://evil.example.com/cb"},
		{"fragment", "https://example.com/cb#x"},
		{"not a url", "://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"redirect_uris": []string{tc.uri}})
			resp, err := h.Client().Post(h.URL+RegisterPath, "application/json", strings.NewReader(string(body)))
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status %d, want 400 for %q", resp.StatusCode, tc.uri)
			}
		})
	}

	t.Run("loopback http is allowed", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"redirect_uris": []string{"http://127.0.0.1:8976/callback"}})
		resp, err := h.Client().Post(h.URL+RegisterPath, "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("status %d, want 201 — local clients need a loopback redirect", resp.StatusCode)
		}
	})
}

func TestRegistrationRequiresARedirectURI(t *testing.T) {
	h := newHarness(t)
	resp, err := h.Client().Post(h.URL+RegisterPath, "application/json", strings.NewReader(`{"client_name":"x"}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

// ─── authorization ───────────────────────────────────────────────

// The redirect URI must be one the client registered, or this endpoint becomes
// an open redirector that lends the server's credibility to a phishing page.
func TestAuthorizeRejectsUnregisteredRedirectURI(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {"https://evil.example.com/steal"},
		"code_challenge":        {challengeFor(testVerifier)},
		"code_challenge_method": {"S256"},
	}
	resp, err := h.noRedirectClient().Get(h.URL + AuthorizePath + "?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("the request was redirected to %q — this endpoint must not redirect to an unregistered address", loc)
	}
}

func TestAuthorizeRejectsUnknownClient(t *testing.T) {
	h := newHarness(t)
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {"not-a-real-client"},
		"redirect_uri":          {testRedirectURI},
		"code_challenge":        {challengeFor(testVerifier)},
		"code_challenge_method": {"S256"},
	}
	resp, err := h.noRedirectClient().Get(h.URL + AuthorizePath + "?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

// PKCE is mandatory under OAuth 2.1, and it is the only thing protecting a code
// in transit back through the browser.
func TestAuthorizeRequiresPKCE(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)

	for _, tc := range []struct {
		name  string
		extra url.Values
	}{
		{"no challenge", url.Values{}},
		{"plain method", url.Values{"code_challenge": {"abc"}, "code_challenge_method": {"plain"}}},
		{"no method", url.Values{"code_challenge": {"abc"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{
				"response_type": {"code"},
				"client_id":     {clientID},
				"redirect_uri":  {testRedirectURI},
				"state":         {"s"},
			}
			for k, v := range tc.extra {
				q[k] = v
			}
			resp, err := h.noRedirectClient().Get(h.URL + AuthorizePath + "?" + q.Encode())
			if err != nil {
				t.Fatalf("authorize: %v", err)
			}
			defer resp.Body.Close()

			// Reported to the client through its verified redirect URI.
			loc, _ := url.Parse(resp.Header.Get("Location"))
			if loc == nil || loc.Query().Get("error") != "invalid_request" {
				t.Errorf("expected invalid_request at the redirect URI, got status %d location %q",
					resp.StatusCode, resp.Header.Get("Location"))
			}
		})
	}
}

// Every MCP client shares this server's single Google client, so Google may
// wave a returning user straight through. The consent screen is the only point
// at which the user is told which client is asking.
func TestAuthorizeShowsConsentBeforeFederating(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {testRedirectURI},
		"code_challenge":        {challengeFor(testVerifier)},
		"code_challenge_method": {"S256"},
	}
	resp, err := h.noRedirectClient().Get(h.URL + AuthorizePath + "?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want the consent page rather than a redirect", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("federated to %q without asking the user", loc)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Test Client") {
		t.Error("the consent page does not name the requesting client")
	}
	if !strings.Contains(body, testRedirectURI) {
		t.Error("the consent page does not show where the user will be sent back to")
	}
	if resp.Header.Get("X-Frame-Options") == "" {
		t.Error("the consent page can be framed, which allows clickjacking")
	}
}

func TestConsentDeclineReportsAccessDenied(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)
	client := h.noRedirectClient()

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {testRedirectURI},
		"code_challenge":        {challengeFor(testVerifier)},
		"code_challenge_method": {"S256"},
		"state":                 {"client-state"},
	}
	resp, err := client.Get(h.URL + AuthorizePath + "?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer resp.Body.Close()
	pending := extractPending(t, readBody(t, resp))

	resp, err = client.PostForm(h.URL+ConsentPath, url.Values{"pending": {pending}, "approve": {"no"}})
	if err != nil {
		t.Fatalf("consent: %v", err)
	}
	defer resp.Body.Close()

	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc == nil || loc.Query().Get("error") != "access_denied" {
		t.Errorf("declining did not report access_denied, got %q", resp.Header.Get("Location"))
	}
	if loc != nil && loc.Query().Get("state") != "client-state" {
		t.Error("the client's state was not returned with the error")
	}
}

// ─── token exchange ──────────────────────────────────────────────

func TestFullAuthorizationCodeFlow(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)
	code := h.authorize(t, clientID, challengeFor(testVerifier))

	resp, out := h.exchange(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"redirect_uri":  {testRedirectURI},
		"client_id":     {clientID},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token status %d: %v", resp.StatusCode, out)
	}
	if out["token_type"] != "Bearer" || out["access_token"] == "" {
		t.Fatalf("unexpected token response: %v", out)
	}
	if out["refresh_token"] == nil {
		t.Error("no refresh token issued — the connection would die within the hour")
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("token responses must not be cached")
	}

	// The issued token must authenticate an MCP request and resolve to the
	// user's own Google credential.
	req := httptest.NewRequest(http.MethodPost, h.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+out["access_token"].(string))
	session, err := h.provider.Authenticate(req)
	if err != nil {
		t.Fatalf("the issued token does not authenticate: %v", err)
	}
	if session.Email != "user@example.com" {
		t.Errorf("session email %q", session.Email)
	}
	token, err := h.provider.GoogleAccessToken(context.Background(), session)
	if err != nil {
		t.Fatalf("GoogleAccessToken: %v", err)
	}
	if token != "google-access-token" {
		t.Errorf("resolved Google token %q", token)
	}
}

func TestTokenRejectsBadPKCEVerifier(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)
	code := h.authorize(t, clientID, challengeFor(testVerifier))

	resp, out := h.exchange(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {"the-wrong-verifier"},
		"redirect_uri":  {testRedirectURI},
	})
	if resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("status %d, body %v — a mismatched verifier must be rejected", resp.StatusCode, out)
	}
}

func TestTokenRequiresAVerifier(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)
	code := h.authorize(t, clientID, challengeFor(testVerifier))

	resp, out := h.exchange(t, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {testRedirectURI},
	})
	if resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("status %d, body %v — a missing verifier must be rejected", resp.StatusCode, out)
	}
}

// A code that appears twice may have been intercepted; the second use is
// refused rather than quietly minting another session.
func TestAuthorizationCodeIsSingleUse(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)
	code := h.authorize(t, clientID, challengeFor(testVerifier))

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"redirect_uri":  {testRedirectURI},
	}
	if resp, out := h.exchange(t, form); resp.StatusCode != http.StatusOK {
		t.Fatalf("first exchange failed: %d %v", resp.StatusCode, out)
	}
	resp, out := h.exchange(t, form)
	if resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("the code was redeemed twice: %d %v", resp.StatusCode, out)
	}
}

func TestTokenRejectsMismatchedRedirectURIAndClient(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)

	t.Run("redirect_uri", func(t *testing.T) {
		code := h.authorize(t, clientID, challengeFor(testVerifier))
		resp, out := h.exchange(t, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"code_verifier": {testVerifier},
			"redirect_uri":  {"https://chatgpt.com/other"},
		})
		if resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_grant" {
			t.Errorf("status %d, body %v", resp.StatusCode, out)
		}
	})

	t.Run("client_id", func(t *testing.T) {
		code := h.authorize(t, clientID, challengeFor(testVerifier))
		other := h.register(t, "https://claude.ai/api/mcp/auth_callback")
		resp, out := h.exchange(t, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"code_verifier": {testVerifier},
			"redirect_uri":  {testRedirectURI},
			"client_id":     {other},
		})
		if resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_grant" {
			t.Errorf("status %d, body %v — a code must not be redeemable by another client", resp.StatusCode, out)
		}
	})
}

func TestUnsupportedGrantType(t *testing.T) {
	h := newHarness(t)
	resp, out := h.exchange(t, url.Values{"grant_type": {"password"}})
	if resp.StatusCode != http.StatusBadRequest || out["error"] != "unsupported_grant_type" {
		t.Errorf("status %d, body %v", resp.StatusCode, out)
	}
}

// OAuth 2.1 requires refresh token rotation for public clients.
func TestRefreshRotatesAndKeepsWorking(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)
	code := h.authorize(t, clientID, challengeFor(testVerifier))

	_, first := h.exchange(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"redirect_uri":  {testRedirectURI},
	})

	resp, second := h.exchange(t, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {first["refresh_token"].(string)},
		"client_id":     {clientID},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh failed: %d %v", resp.StatusCode, second)
	}
	if second["refresh_token"] == first["refresh_token"] {
		t.Error("the refresh token was not rotated")
	}
	if second["access_token"] == first["access_token"] {
		t.Error("refreshing returned the same access token")
	}

	req := httptest.NewRequest(http.MethodPost, h.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+second["access_token"].(string))
	if _, err := h.provider.Authenticate(req); err != nil {
		t.Errorf("the rotated access token does not authenticate: %v", err)
	}
}

func TestRefreshRejectsAnInvalidToken(t *testing.T) {
	h := newHarness(t)
	resp, out := h.exchange(t, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"nonsense"},
	})
	if resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Errorf("status %d, body %v", resp.StatusCode, out)
	}
}

// ─── resource-server behaviour ───────────────────────────────────

func TestAuthenticateRejectsBadTokens(t *testing.T) {
	h := newHarness(t)

	tests := []struct{ name, header string }{
		{"missing", ""},
		{"not bearer", "Basic abc"},
		{"garbage", "Bearer not-a-token"},
		{"empty bearer", "Bearer "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, h.URL+"/mcp", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if _, err := h.provider.Authenticate(req); err == nil {
				t.Error("expected authentication to fail")
			}
		})
	}
}

// A refresh token must never be accepted as an access token: they are sealed
// under different kinds precisely so one cannot stand in for the other.
func TestRefreshTokenIsNotAnAccessToken(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)
	code := h.authorize(t, clientID, challengeFor(testVerifier))
	_, out := h.exchange(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"redirect_uri":  {testRedirectURI},
	})

	req := httptest.NewRequest(http.MethodPost, h.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+out["refresh_token"].(string))
	if _, err := h.provider.Authenticate(req); err == nil {
		t.Fatal("a refresh token was accepted as an access token")
	}
}

func TestExpiredAccessTokenIsRejected(t *testing.T) {
	h := newHarness(t)
	expired, err := h.provider.sealer.seal(kindAccess, sessionPayload{
		Refresh:   "google-refresh-token",
		Resource:  h.URL + "/mcp",
		IssuedAt:  time.Now().Add(-2 * time.Hour).Unix(),
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, h.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+expired)
	if _, err := h.provider.Authenticate(req); err == nil {
		t.Fatal("an expired token was accepted")
	}
}

// Audience binding (RFC 8707): a token minted for another deployment must not
// work here, even though the same key sealed it.
func TestTokenForAnotherResourceIsRejected(t *testing.T) {
	h := newHarness(t)
	foreign, err := h.provider.sealer.seal(kindAccess, sessionPayload{
		Refresh:   "google-refresh-token",
		Resource:  "https://someone-elses-server.example.com/mcp",
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, h.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+foreign)
	if _, err := h.provider.Authenticate(req); err == nil {
		t.Fatal("a token issued for a different resource was accepted")
	}
}

// A 401 has to point at the resource metadata, or the client has no way to
// discover where to authenticate and the connector just fails.
func TestChallengeHeaderPointsAtDiscovery(t *testing.T) {
	h := newHarness(t)
	mux := http.NewServeMux()
	h.provider.Register(mux)
	mux.Handle("/mcp", h.provider.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := srv.Client().Post(srv.URL+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(challenge, ProtectedResourcePath) {
		t.Errorf("WWW-Authenticate %q does not point at the resource metadata", challenge)
	}
}

// Two users must never see each other's data. The session is what carries
// identity, so this checks the credential resolved for each is their own.
func TestSessionsAreIsolatedPerUser(t *testing.T) {
	h := newHarness(t)

	mint := func(refresh, email string) *Session {
		t.Helper()
		tok, err := h.provider.sealer.seal(kindAccess, sessionPayload{
			Refresh:   refresh,
			Email:     email,
			Resource:  h.URL + "/mcp",
			IssuedAt:  time.Now().Unix(),
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, h.URL+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		session, err := h.provider.Authenticate(req)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		return session
	}

	alice := mint("alice-refresh", "alice@example.com")
	bob := mint("bob-refresh", "bob@example.com")

	if alice.googleRefresh == bob.googleRefresh {
		t.Fatal("two sessions resolved to the same Google credential")
	}
	if alice.Email == bob.Email {
		t.Fatal("two sessions resolved to the same account")
	}
}

// The cache is what keeps a tool call from paying a Google round trip every
// time; without it every request would refresh.
func TestGoogleAccessTokenIsCached(t *testing.T) {
	h := newHarness(t)
	session := &Session{googleRefresh: "some-refresh-token"}

	for i := range 3 {
		if _, err := h.provider.GoogleAccessToken(context.Background(), session); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if h.google.refreshCount != 1 {
		t.Errorf("refreshed %d times for three calls, want 1", h.google.refreshCount)
	}
}

func TestGoogleAccessTokenRequiresASession(t *testing.T) {
	h := newHarness(t)
	if _, err := h.provider.GoogleAccessToken(context.Background(), nil); err == nil {
		t.Error("a nil session resolved to a token")
	}
	if _, err := h.provider.GoogleAccessToken(context.Background(), &Session{}); err == nil {
		t.Error("an empty session resolved to a token")
	}
}

// Without a refresh token the connection dies in an hour with no way to renew,
// so this is reported at sign-in rather than as a mysterious failure later.
func TestSignInFailsWithoutARefreshToken(t *testing.T) {
	h := newHarness(t)
	h.google.omitRefresh = true
	clientID := h.register(t)
	client := h.noRedirectClient()

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {testRedirectURI},
		"code_challenge":        {challengeFor(testVerifier)},
		"code_challenge_method": {"S256"},
	}
	resp, _ := client.Get(h.URL + AuthorizePath + "?" + q.Encode())
	pending := extractPending(t, readBody(t, resp))
	resp.Body.Close()

	resp, _ = client.PostForm(h.URL+ConsentPath, url.Values{"pending": {pending}, "approve": {"yes"}})
	googleURL := resp.Header.Get("Location")
	resp.Body.Close()

	resp, _ = client.Get(googleURL)
	callbackURL := resp.Header.Get("Location")
	resp.Body.Close()

	resp, err := client.Get(callbackURL)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 400 {
		t.Fatalf("status %d, want an error page", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "refresh token") {
		t.Errorf("the error does not explain the cause:\n%s", body)
	}
}

// Google only reliably returns a refresh token when both of these are set.
func TestGoogleConsentIsForcedOffline(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)
	h.authorize(t, clientID, challengeFor(testVerifier))

	if got := h.google.lastAuthReq.Get("access_type"); got != "offline" {
		t.Errorf("access_type %q, want offline", got)
	}
	if got := h.google.lastAuthReq.Get("prompt"); got != "consent" {
		t.Errorf("prompt %q, want consent", got)
	}
}

// ─── configuration ───────────────────────────────────────────────

func TestNewProviderValidatesConfig(t *testing.T) {
	valid := Config{Secret: testSecret, Google: GoogleConfig{ClientID: "id", ClientSecret: "secret"}}

	t.Run("weak secret", func(t *testing.T) {
		cfg := valid
		cfg.Secret = "too-short"
		if _, err := NewProvider(cfg); err == nil {
			t.Error("a short secret was accepted")
		}
	})
	t.Run("missing Google client", func(t *testing.T) {
		cfg := valid
		cfg.Google.ClientSecret = ""
		if _, err := NewProvider(cfg); err == nil {
			t.Error("a missing Google client secret was accepted")
		}
	})
	t.Run("relative public URL", func(t *testing.T) {
		cfg := valid
		cfg.PublicURL = "example.com"
		if _, err := NewProvider(cfg); err == nil {
			t.Error("a non-absolute public URL was accepted")
		}
	})
	t.Run("valid", func(t *testing.T) {
		cfg := valid
		cfg.PublicURL = "https://example.com/"
		p, err := NewProvider(cfg)
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		if p.publicURL != "https://example.com" {
			t.Errorf("public URL %q — the trailing slash should be trimmed", p.publicURL)
		}
	})
}

// Rotating the secret must invalidate everything it sealed; that is the only
// revocation mechanism a stateless design has.
func TestRotatingTheSecretInvalidatesCredentials(t *testing.T) {
	h := newHarness(t)
	clientID := h.register(t)
	code := h.authorize(t, clientID, challengeFor(testVerifier))
	_, out := h.exchange(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
		"redirect_uri":  {testRedirectURI},
	})

	rotated, err := newSealer("ffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("newSealer: %v", err)
	}
	h.provider.sealer = rotated

	req := httptest.NewRequest(http.MethodPost, h.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+out["access_token"].(string))
	if _, err := h.provider.Authenticate(req); err == nil {
		t.Error("a token sealed with the old secret still authenticates")
	}
}

func TestDefaultScopesAreReadOnly(t *testing.T) {
	scopes := DefaultScopes()
	if len(scopes) < 3 {
		t.Fatalf("expected the health read scopes, got %v", scopes)
	}
	for _, s := range scopes {
		if s == "openid" || s == "email" {
			continue
		}
		if !strings.HasSuffix(s, ".readonly") {
			t.Errorf("scope %q grants more than read access — the MCP surface is read-only", s)
		}
	}
}
