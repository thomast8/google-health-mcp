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

package mcpserver_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"ghealth/pkg/auth"
	"ghealth/pkg/mcpauth"
	"ghealth/pkg/mcpserver"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// This is the proof that multi-user mode works end to end: two people complete
// the real OAuth flow against the real server, each tool call runs the real CLI
// binary as a child process, and each sees only their own data.
//
// The only stubs are the two external services — Google's OAuth endpoints and
// the Health API — because a test cannot sign in as a real person.

// ─── stub Google ─────────────────────────────────────────────────

type fakeGoogle struct {
	*httptest.Server
	mu   sync.Mutex
	user string // the account the next authorization belongs to
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	g := &fakeGoogle{}
	mux := http.NewServeMux()

	// Google's consent screen: redirects straight back with a per-user code.
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		user := g.user
		g.mu.Unlock()

		redirect, _ := url.Parse(r.URL.Query().Get("redirect_uri"))
		q := redirect.Query()
		q.Set("code", user+"-google-code")
		q.Set("state", r.URL.Query().Get("state"))
		redirect.RawQuery = q.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	})

	// Tokens are named after the user so the Health API stub can tell whose
	// credential arrived.
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		var user string
		switch r.PostFormValue("grant_type") {
		case "authorization_code":
			user = strings.TrimSuffix(r.PostFormValue("code"), "-google-code")
		case "refresh_token":
			user = strings.TrimSuffix(r.PostFormValue("refresh_token"), "-google-refresh")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  user + "-google-access",
			"refresh_token": user + "-google-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})

	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGoogle) signInAs(user string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.user = user
}

// ─── stub Health API ─────────────────────────────────────────────

// newFakeHealthAPI serves a step count that identifies the bearer token it was
// called with, so a test can tell whose credential the CLI actually used.
func newFakeHealthAPI(t *testing.T, stepsByToken map[string]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		steps, known := stepsByToken[token]
		if !known {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"error":{"code":401,"message":"unknown credential %q"}}`, token)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"rollupDataPoints":[{"startTime":"2026-03-22T00:00:00Z","endTime":"2026-03-23T00:00:00Z","steps":{"countSum":"%d"}}]}`, steps)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ─── the server under test ───────────────────────────────────────

type oauthHarness struct {
	*httptest.Server
	google *fakeGoogle
}

func newOAuthHarness(t *testing.T, stepsByToken map[string]int) *oauthHarness {
	t.Helper()

	google := newFakeGoogle(t)
	healthAPI := newFakeHealthAPI(t, stepsByToken)

	// The CLI child reads the API base URL from the environment it inherits.
	t.Setenv("GHEALTH_BASE_URL", healthAPI.URL)

	provider, err := mcpauth.NewProvider(mcpauth.Config{
		Secret:  "0123456789abcdef0123456789abcdef",
		MCPPath: mcpserver.MCPPath,
		Google: mcpauth.GoogleConfig{
			ClientID:     "test-client",
			ClientSecret: "test-secret",
			Endpoint:     oauth2.Endpoint{AuthURL: google.URL + "/auth", TokenURL: google.URL + "/token"},
			Scopes:       []string{"openid", "email"},
			TokenInfo: func(_ context.Context, accessToken string) (*auth.TokenInfo, error) {
				user := strings.TrimSuffix(accessToken, "-google-access")
				return &auth.TokenInfo{Email: user + "@example.com"}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	// The real runner, invoking the real binary as the authenticated caller.
	runner := &mcpserver.ExecRunner{
		Binary:         ghealthBinary(t),
		PerRequestAuth: true,
		ConfigDir:      t.TempDir(),
	}
	mcpSrv := mcpserver.New(mcpserver.Options{Runner: runner, Version: "test"})

	handler, err := mcpserver.Handler(mcpSrv, mcpserver.HTTPOptions{OAuth: provider})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	h := &oauthHarness{google: google}
	h.Server = httptest.NewServer(handler)
	t.Cleanup(h.Close)
	return h
}

func (h *oauthHarness) client() *http.Client {
	c := *h.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// connectAs walks the whole OAuth flow as a user and returns the access token
// an MCP client would end up holding.
func (h *oauthHarness) connectAs(t *testing.T, user string) string {
	t.Helper()
	h.google.signInAs(user)
	client := h.client()

	// 1. Discover the authorization server.
	resp, err := h.Client().Get(h.URL + mcpauth.ProtectedResourcePath)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	json.NewDecoder(resp.Body).Decode(&prm)
	resp.Body.Close()
	if len(prm.AuthorizationServers) == 0 {
		t.Fatal("no authorization server advertised")
	}

	// 2. Register dynamically, as ChatGPT and Claude do.
	redirectURI := "https://chatgpt.com/connector_platform_oauth_redirect"
	body, _ := json.Marshal(map[string]any{
		"redirect_uris": []string{redirectURI},
		"client_name":   "ChatGPT",
	})
	resp, err = h.Client().Post(prm.AuthorizationServers[0]+mcpauth.RegisterPath, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()

	// 3. Authorize, with PKCE.
	verifier := "verifier-for-" + user + "-long-enough-for-pkce"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {reg.ClientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {user + "-state"},
		"resource":              {h.URL + mcpserver.MCPPath},
	}
	resp, err = client.Get(h.URL + mcpauth.AuthorizePath + "?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	page := readAll(t, resp)
	resp.Body.Close()

	// 4. Approve on this server's own consent screen.
	pending := fieldValue(t, page, "pending")
	resp, err = client.PostForm(h.URL+mcpauth.ConsentPath, url.Values{"pending": {pending}, "approve": {"yes"}})
	if err != nil {
		t.Fatalf("consent: %v", err)
	}
	googleURL := resp.Header.Get("Location")
	resp.Body.Close()

	// 5. Sign in at Google, which redirects back to the callback.
	resp, err = client.Get(googleURL)
	if err != nil {
		t.Fatalf("google: %v", err)
	}
	callback := resp.Header.Get("Location")
	resp.Body.Close()

	resp, err = client.Get(callback)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	final, _ := url.Parse(resp.Header.Get("Location"))
	resp.Body.Close()
	if final == nil || final.Query().Get("code") == "" {
		t.Fatalf("no authorization code returned for %s", user)
	}

	// 6. Exchange the code for a token.
	resp, err = h.Client().PostForm(h.URL+mcpauth.TokenPath, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {final.Query().Get("code")},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
		"client_id":     {reg.ClientID},
		"resource":      {h.URL + mcpserver.MCPPath},
	})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if tok.AccessToken == "" {
		t.Fatalf("no access token issued for %s", user)
	}
	return tok.AccessToken
}

// mcpSession connects an MCP client to the server with a bearer token.
func (h *oauthHarness) mcpSession(t *testing.T, accessToken string) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint: h.URL + mcpserver.MCPPath,
		HTTPClient: &http.Client{
			Transport: bearerTransport{token: accessToken, base: h.Client().Transport},
		},
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	base := b.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func fieldValue(t *testing.T, html, name string) string {
	t.Helper()
	marker := `name="` + name + `" value="`
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatalf("no %q field in:\n%s", name, html)
	}
	rest := html[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("field %q is unterminated", name)
	}
	return rest[:j]
}

// ─── the tests ───────────────────────────────────────────────────

// Two users connect to the same deployment and each sees their own steps. If
// tenant isolation were broken anywhere along the chain — session, token
// resolution, subprocess environment — these numbers would collide.
func TestOAuthTwoUsersSeeTheirOwnData(t *testing.T) {
	h := newOAuthHarness(t, map[string]int{
		"alice-google-access": 11111,
		"bob-google-access":   22222,
	})

	aliceToken := h.connectAs(t, "alice")
	bobToken := h.connectAs(t, "bob")
	if aliceToken == bobToken {
		t.Fatal("both users were issued the same access token")
	}

	steps := func(accessToken string) string {
		session := h.mcpSession(t, accessToken)
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "query_data",
			Arguments: map[string]any{
				"data_type": "steps",
				"operation": "daily-rollup",
				"from":      "2026-03-22",
				"to":        "2026-03-23",
			},
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		if res.IsError {
			t.Fatalf("tool error: %s", text)
		}
		return text
	}

	aliceData, bobData := steps(aliceToken), steps(bobToken)

	if !strings.Contains(aliceData, "11111") {
		t.Errorf("alice did not get her own steps:\n%s", aliceData)
	}
	if strings.Contains(aliceData, "22222") {
		t.Errorf("alice was shown bob's data:\n%s", aliceData)
	}
	if !strings.Contains(bobData, "22222") {
		t.Errorf("bob did not get his own steps:\n%s", bobData)
	}
	if strings.Contains(bobData, "11111") {
		t.Errorf("bob was shown alice's data:\n%s", bobData)
	}
}

// auth_status must name the account, so a user can confirm which Google
// account a connector ended up signed in as.
func TestOAuthAuthStatusNamesTheSignedInAccount(t *testing.T) {
	h := newOAuthHarness(t, map[string]int{"alice-google-access": 1})
	session := h.mcpSession(t, h.connectAs(t, "alice"))

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "auth_status"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(got, "alice@example.com") {
		t.Errorf("auth_status does not name the account:\n%s", got)
	}
}

// An unauthenticated MCP request must be refused and must point the client at
// discovery, which is how a connector knows to start the OAuth flow at all.
func TestOAuthUnauthenticatedRequestStartsDiscovery(t *testing.T) {
	h := newOAuthHarness(t, nil)

	resp, err := h.Client().Post(h.URL+mcpserver.MCPPath, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(challenge, mcpauth.ProtectedResourcePath) {
		t.Errorf("WWW-Authenticate %q does not point at the resource metadata", challenge)
	}
}

// A revoked or unknown Google credential must surface as a tool error naming
// the problem, not as a silent empty result or a fall back to another account.
func TestOAuthUnknownGoogleCredentialFails(t *testing.T) {
	// The Health API stub knows nobody, so every credential is rejected.
	h := newOAuthHarness(t, map[string]int{})
	session := h.mcpSession(t, h.connectAs(t, "alice"))

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "query_data",
		Arguments: map[string]any{"data_type": "steps", "operation": "list"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected a tool error, got:\n%s", res.Content[0].(*mcp.TextContent).Text)
	}
}

// The health check stays open so the container host's probe keeps working once
// Google sign-in is enabled.
func TestOAuthHealthCheckStaysOpen(t *testing.T) {
	h := newOAuthHarness(t, nil)
	resp, err := h.Client().Get(h.URL + mcpserver.HealthPath)
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
}

// The consent form is what stands between a user and a client they never
// chose, so a cross-site POST to it must be refused.
func TestOAuthConsentRejectsCrossSitePost(t *testing.T) {
	h := newOAuthHarness(t, nil)

	req, err := http.NewRequest(http.MethodPost, h.URL+mcpauth.ConsentPath,
		strings.NewReader(url.Values{"pending": {"x"}, "approve": {"yes"}}.Encode()))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403 — a cross-site approval must not be honoured", resp.StatusCode)
	}
}
