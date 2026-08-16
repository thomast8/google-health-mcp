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
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Endpoint paths. These are advertised through the metadata documents, so a
// client never hardcodes them.
const (
	ProtectedResourcePath = "/.well-known/oauth-protected-resource"
	MetadataPath          = "/.well-known/oauth-authorization-server"
	RegisterPath          = "/oauth/register"
	AuthorizePath         = "/oauth/authorize"
	ConsentPath           = "/oauth/consent"
	CallbackPath          = "/oauth/callback"
	TokenPath             = "/oauth/token"
)

// Scope is the single scope this server issues. Google's own scopes are an
// upstream detail the MCP client has no say in.
const Scope = "health.readonly"

// Credential lifetimes.
const (
	defaultAccessTTL  = time.Hour
	defaultRefreshTTL = 90 * 24 * time.Hour
	pendingTTL        = 15 * time.Minute
	codeTTL           = 2 * time.Minute
)

// Config configures a Provider.
type Config struct {
	// Secret seals every credential the server issues. Rotating it revokes
	// all of them.
	Secret string
	// Google is the server's own OAuth client at Google.
	Google GoogleConfig
	// PublicURL is the externally reachable origin, e.g.
	// https://ghealth.up.railway.app. When empty it is derived per request
	// from the forwarded headers, which works but is worth setting explicitly.
	PublicURL string
	// MCPPath is the protected resource's path. Defaults to /mcp.
	MCPPath string
	// AccessTTL and RefreshTTL override the credential lifetimes.
	AccessTTL, RefreshTTL time.Duration
}

// Provider is the OAuth 2.1 authorization server and the MCP resource server's
// token validator.
type Provider struct {
	sealer     *sealer
	google     GoogleConfig
	publicURL  string
	mcpPath    string
	tokens     *tokenCache
	codes      *replayGuard
	accessTTL  time.Duration
	refreshTTL time.Duration
}

// NewProvider validates the configuration and builds a Provider.
func NewProvider(cfg Config) (*Provider, error) {
	s, err := newSealer(cfg.Secret)
	if err != nil {
		return nil, err
	}
	if cfg.Google.ClientID == "" || cfg.Google.ClientSecret == "" {
		return nil, fmt.Errorf("a Google OAuth client ID and secret are required for Google sign-in")
	}
	if cfg.PublicURL != "" {
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("the public URL must be absolute, e.g. https://example.up.railway.app (got %q)", cfg.PublicURL)
		}
	}

	p := &Provider{
		sealer:     s,
		google:     cfg.Google,
		publicURL:  strings.TrimRight(cfg.PublicURL, "/"),
		mcpPath:    cfg.MCPPath,
		tokens:     newTokenCache(),
		codes:      newReplayGuard(),
		accessTTL:  cfg.AccessTTL,
		refreshTTL: cfg.RefreshTTL,
	}
	if p.mcpPath == "" {
		p.mcpPath = "/mcp"
	}
	if p.accessTTL <= 0 {
		p.accessTTL = defaultAccessTTL
	}
	if p.refreshTTL <= 0 {
		p.refreshTTL = defaultRefreshTTL
	}
	return p, nil
}

// Register mounts every OAuth endpoint on the mux.
func (p *Provider) Register(mux *http.ServeMux) {
	// Clients probe both the bare metadata paths and the variants with the
	// resource path appended (RFC 9728 §3.1, RFC 8414 §3.1). Serve both rather
	// than depending on which form a given client tries.
	for _, path := range []string{ProtectedResourcePath, ProtectedResourcePath + p.mcpPath} {
		mux.HandleFunc("GET "+path, p.handleProtectedResource)
	}
	for _, path := range []string{MetadataPath, MetadataPath + p.mcpPath} {
		mux.HandleFunc("GET "+path, p.handleMetadata)
	}
	mux.HandleFunc("POST "+RegisterPath, p.handleRegister)
	mux.HandleFunc("GET "+AuthorizePath, p.handleAuthorize)
	mux.HandleFunc("POST "+ConsentPath, p.handleConsent)
	mux.HandleFunc("GET "+CallbackPath, p.handleCallback)
	mux.HandleFunc("POST "+TokenPath, p.handleToken)
}

// baseURL is the origin to build absolute URLs from.
func (p *Provider) baseURL(r *http.Request) string {
	if p.publicURL != "" {
		return p.publicURL
	}
	scheme := "https"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.TrimSpace(strings.Split(proto, ",")[0])
	} else if r.TLS == nil && isLoopbackHost(r.Host) {
		scheme = "http"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	return scheme + "://" + host
}

func isLoopbackHost(host string) bool {
	h := host
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		h = host[:i]
	}
	h = strings.Trim(h, "[]")
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// resourceURI is the canonical identifier of the protected MCP endpoint, used
// for audience binding (RFC 8707).
func (p *Provider) resourceURI(r *http.Request) string {
	return p.baseURL(r) + p.mcpPath
}

// ─── metadata ────────────────────────────────────────────────────

func (p *Provider) handleProtectedResource(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 p.resourceURI(r),
		"authorization_servers":    []string{p.baseURL(r)},
		"scopes_supported":         []string{Scope},
		"bearer_methods_supported": []string{"header"},
	})
}

func (p *Provider) handleMetadata(w http.ResponseWriter, r *http.Request) {
	base := p.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + AuthorizePath,
		"token_endpoint":                        base + TokenPath,
		"registration_endpoint":                 base + RegisterPath,
		"scopes_supported":                      []string{Scope},
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"service_documentation":                 "https://github.com/thomast8/google-health-mcp",
	})
}

// ─── dynamic client registration (RFC 7591) ──────────────────────

type registrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

// handleRegister issues a client ID to any caller. Open registration is what
// lets ChatGPT and Claude connect without the operator pre-provisioning them,
// and it is safe here because a client ID grants nothing on its own: every
// authorization still needs the user to sign in at Google and approve this
// server's own consent screen, which names the client and where it will send
// the user back.
func (p *Provider) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registrationRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "the registration body is not valid JSON")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "at least one redirect_uri is required")
		return
	}
	for _, uri := range req.RedirectURIs {
		if err := validateRedirectURI(uri); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}

	clientID, err := p.sealer.seal(kindClient, clientPayload{
		RedirectURIs: req.RedirectURIs,
		Name:         truncate(req.ClientName, 120),
		IssuedAt:     time.Now().Unix(),
	})
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not issue a client ID")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        time.Now().Unix(),
		"redirect_uris":              req.RedirectURIs,
		"client_name":                req.ClientName,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      Scope,
	})
}

// validateRedirectURI enforces OAuth 2.1's transport rule: HTTPS everywhere,
// except loopback, which local clients need and which never leaves the machine.
func validateRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("redirect_uri %q is not a valid URL", raw)
	}
	if u.Fragment != "" {
		return fmt.Errorf("redirect_uri %q must not contain a fragment", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Host) {
			return nil
		}
		return fmt.Errorf("redirect_uri %q must use https (plain http is allowed only for loopback)", raw)
	default:
		// Custom schemes are how native apps receive redirects; they are
		// legitimate, but they cannot be verified, so a host must be present.
		if u.Scheme != "" && u.Opaque == "" && u.Host != "" {
			return nil
		}
		return fmt.Errorf("redirect_uri %q must use https or a loopback address", raw)
	}
}

// ─── authorization ───────────────────────────────────────────────

type clientPayload struct {
	RedirectURIs []string `json:"r"`
	Name         string   `json:"n,omitempty"`
	IssuedAt     int64    `json:"i"`
}

// pendingPayload is the state carried through this server's consent screen and
// on to Google, then back again. Sealing it means none of it can be tampered
// with in the browser.
type pendingPayload struct {
	RedirectURI string `json:"r"`
	ClientState string `json:"s,omitempty"`
	Challenge   string `json:"cc"`
	ClientHash  string `json:"ch"`
	ClientName  string `json:"cn,omitempty"`
	Resource    string `json:"re,omitempty"`
	IssuedAt    int64  `json:"i"`
}

func (p *Provider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// The client must be identified and its redirect URI verified before
	// anything can be sent back to it; until then, errors render here rather
	// than redirect, or an attacker could use this endpoint as an open
	// redirector.
	var client clientPayload
	if err := p.sealer.open(kindClient, q.Get("client_id"), &client); err != nil {
		renderError(w, http.StatusBadRequest, "Unrecognised client",
			"This client is not registered with this server, or the server's signing secret has been rotated. Reconnect the connector to register again.")
		return
	}
	redirectURI := q.Get("redirect_uri")
	if !slicesContain(client.RedirectURIs, redirectURI) {
		renderError(w, http.StatusBadRequest, "Redirect address not registered",
			"The client asked to be sent back to an address it did not register. Nothing has been shared.")
		return
	}

	// From here failures are reported to the client, per OAuth.
	state := q.Get("state")
	if rt := q.Get("response_type"); rt != "code" {
		redirectError(w, r, redirectURI, state, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	challenge := q.Get("code_challenge")
	if challenge == "" {
		redirectError(w, r, redirectURI, state, "invalid_request", "code_challenge is required (PKCE)")
		return
	}
	if method := q.Get("code_challenge_method"); method != "S256" {
		redirectError(w, r, redirectURI, state, "invalid_request", "code_challenge_method must be S256")
		return
	}
	// RFC 8707: the token is bound to one resource. Accept the client's value
	// when it names this server, and fall back to ours when it is absent.
	resource := q.Get("resource")
	if resource != "" && !sameResource(resource, p.resourceURI(r)) {
		redirectError(w, r, redirectURI, state, "invalid_target",
			"the resource parameter does not identify this MCP server")
		return
	}

	pending, err := p.sealer.seal(kindPending, pendingPayload{
		RedirectURI: redirectURI,
		ClientState: state,
		Challenge:   challenge,
		ClientHash:  hashClientID(q.Get("client_id")),
		ClientName:  client.Name,
		Resource:    p.resourceURI(r),
		IssuedAt:    time.Now().Unix(),
	})
	if err != nil {
		redirectError(w, r, redirectURI, state, "server_error", "could not start the authorization")
		return
	}

	// The confused-deputy mitigation the MCP spec requires. Every MCP client
	// shares this server's single Google OAuth client, so Google may already
	// have consent recorded for it and would wave the user straight through.
	// Asking here — naming the client and where it will be sent back to —
	// is what stops a client the user never chose from silently obtaining a
	// code on their behalf.
	renderConsent(w, consentView{
		ClientName:  displayName(client.Name),
		RedirectURI: redirectURI,
		Pending:     pending,
		Action:      ConsentPath,
	})
}

// handleConsent runs after the user approves, and federates to Google.
func (p *Provider) handleConsent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		renderError(w, http.StatusBadRequest, "Malformed request", "The approval form could not be read.")
		return
	}
	raw := r.PostFormValue("pending")

	var pending pendingPayload
	if err := p.sealer.open(kindPending, raw, &pending); err != nil {
		renderError(w, http.StatusBadRequest, "Expired request", "Start the connection again from your client.")
		return
	}
	if expired(pending.IssuedAt, pendingTTL) {
		renderError(w, http.StatusBadRequest, "Expired request", "This approval took too long. Start the connection again from your client.")
		return
	}
	if r.PostFormValue("approve") != "yes" {
		redirectError(w, r, pending.RedirectURI, pending.ClientState, "access_denied", "the user declined")
		return
	}

	http.Redirect(w, r, p.google.authCodeURL(p.baseURL(r)+CallbackPath, raw), http.StatusFound)
}

// ─── Google callback ─────────────────────────────────────────────

type codePayload struct {
	Refresh     string `json:"gr"`
	Email       string `json:"e,omitempty"`
	RedirectURI string `json:"r"`
	Challenge   string `json:"cc"`
	ClientHash  string `json:"ch"`
	Resource    string `json:"re,omitempty"`
	IssuedAt    int64  `json:"i"`
}

func (p *Provider) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var pending pendingPayload
	if err := p.sealer.open(kindPending, q.Get("state"), &pending); err != nil {
		renderError(w, http.StatusBadRequest, "Invalid sign-in response",
			"The response from Google could not be matched to a pending request. Start the connection again.")
		return
	}
	if expired(pending.IssuedAt, pendingTTL) {
		renderError(w, http.StatusBadRequest, "Expired sign-in", "Start the connection again from your client.")
		return
	}
	if gerr := q.Get("error"); gerr != "" {
		redirectError(w, r, pending.RedirectURI, pending.ClientState, "access_denied", "Google reported: "+gerr)
		return
	}

	tok, err := p.google.exchange(r.Context(), p.baseURL(r)+CallbackPath, q.Get("code"))
	if err != nil {
		renderError(w, http.StatusBadGateway, "Google sign-in failed", err.Error())
		return
	}
	p.tokens.put(tok.RefreshToken, tok.AccessToken, tok.Expiry)

	code, err := p.sealer.seal(kindCode, codePayload{
		Refresh:     tok.RefreshToken,
		Email:       p.google.accountEmail(r.Context(), tok.AccessToken),
		RedirectURI: pending.RedirectURI,
		Challenge:   pending.Challenge,
		ClientHash:  pending.ClientHash,
		Resource:    pending.Resource,
		IssuedAt:    time.Now().Unix(),
	})
	if err != nil {
		renderError(w, http.StatusInternalServerError, "Could not complete sign-in", "Try connecting again.")
		return
	}

	target, err := url.Parse(pending.RedirectURI)
	if err != nil {
		renderError(w, http.StatusBadRequest, "Invalid redirect address", "The client's redirect address could not be parsed.")
		return
	}
	params := target.Query()
	params.Set("code", code)
	if pending.ClientState != "" {
		params.Set("state", pending.ClientState)
	}
	target.RawQuery = params.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// ─── token endpoint ──────────────────────────────────────────────

type sessionPayload struct {
	Refresh    string `json:"gr"`
	Email      string `json:"e,omitempty"`
	ClientHash string `json:"ch"`
	Resource   string `json:"re,omitempty"`
	IssuedAt   int64  `json:"i"`
	ExpiresAt  int64  `json:"x"`
}

func (p *Provider) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the request body could not be parsed")
		return
	}
	switch grant := r.PostFormValue("grant_type"); grant {
	case "authorization_code":
		p.grantAuthorizationCode(w, r)
	case "refresh_token":
		p.grantRefreshToken(w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type",
			fmt.Sprintf("grant_type %q is not supported", grant))
	}
}

func (p *Provider) grantAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	raw := r.PostFormValue("code")

	var code codePayload
	if err := p.sealer.open(kindCode, raw, &code); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code is not valid")
		return
	}
	if expired(code.IssuedAt, codeTTL) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code has expired")
		return
	}
	// One-time use. A code that reappears may have been intercepted, so the
	// safe reading is to reject it rather than mint a second session.
	if !p.codes.claim(raw, codeTTL) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code has already been used")
		return
	}
	if !verifyPKCE(code.Challenge, r.PostFormValue("code_verifier")) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the PKCE code_verifier does not match the challenge")
		return
	}
	if redirect := r.PostFormValue("redirect_uri"); redirect != "" && redirect != code.RedirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}
	if clientID := r.PostFormValue("client_id"); clientID != "" && hashClientID(clientID) != code.ClientHash {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id does not match the authorization request")
		return
	}

	p.issueSession(w, r, sessionPayload{
		Refresh:    code.Refresh,
		Email:      code.Email,
		ClientHash: code.ClientHash,
		Resource:   code.Resource,
	})
}

func (p *Provider) grantRefreshToken(w http.ResponseWriter, r *http.Request) {
	var session sessionPayload
	if err := p.sealer.open(kindRefresh, r.PostFormValue("refresh_token"), &session); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is not valid")
		return
	}
	if session.ExpiresAt != 0 && time.Now().After(time.Unix(session.ExpiresAt, 0)) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token has expired; sign in again")
		return
	}
	if clientID := r.PostFormValue("client_id"); clientID != "" && hashClientID(clientID) != session.ClientHash {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id does not match the session")
		return
	}
	// Rotate: a new refresh token is issued and the old one is not reusable
	// beyond its own expiry, as OAuth 2.1 requires for public clients.
	p.issueSession(w, r, session)
}

func (p *Provider) issueSession(w http.ResponseWriter, r *http.Request, base sessionPayload) {
	now := time.Now()
	if base.Resource == "" {
		base.Resource = p.resourceURI(r)
	}

	access := base
	access.IssuedAt = now.Unix()
	access.ExpiresAt = now.Add(p.accessTTL).Unix()
	accessToken, err := p.sealer.seal(kindAccess, access)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not issue an access token")
		return
	}

	refresh := base
	refresh.IssuedAt = now.Unix()
	refresh.ExpiresAt = now.Add(p.refreshTTL).Unix()
	refreshToken, err := p.sealer.seal(kindRefresh, refresh)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not issue a refresh token")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  accessToken,
		"token_type":    "Bearer",
		"expires_in":    int(p.accessTTL.Seconds()),
		"refresh_token": refreshToken,
		"scope":         Scope,
	})
}

// ─── resource-server side ────────────────────────────────────────

// Session is an authenticated MCP caller.
type Session struct {
	// Email names the Google account, for display only.
	Email string
	// googleRefresh is unexported: the Google credential must not leak past
	// this package into tool code or logs.
	googleRefresh string
}

// Authenticate validates the bearer token on an MCP request.
func (p *Provider) Authenticate(r *http.Request) (*Session, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return nil, fmt.Errorf("no bearer token")
	}

	var session sessionPayload
	if err := p.sealer.open(kindAccess, strings.TrimSpace(raw), &session); err != nil {
		return nil, fmt.Errorf("invalid access token")
	}
	if session.ExpiresAt != 0 && time.Now().After(time.Unix(session.ExpiresAt, 0)) {
		return nil, fmt.Errorf("the access token has expired")
	}
	// Audience binding: a token minted for a different resource must not be
	// accepted here, even though it was sealed with the same key.
	if session.Resource != "" && !sameResource(session.Resource, p.resourceURI(r)) {
		return nil, fmt.Errorf("the access token was issued for a different resource")
	}
	if session.Refresh == "" {
		return nil, fmt.Errorf("the access token carries no Google credential")
	}
	return &Session{Email: session.Email, googleRefresh: session.Refresh}, nil
}

// GoogleAccessToken exchanges a session for a live Google access token.
//
// This is the only way a Google credential leaves the package, and it hands out
// a short-lived access token rather than the refresh token behind it.
func (p *Provider) GoogleAccessToken(ctx context.Context, s *Session) (string, error) {
	if s == nil || s.googleRefresh == "" {
		return "", fmt.Errorf("not authenticated")
	}
	return p.accessTokenFor(ctx, s.googleRefresh)
}

// ChallengeHeader is the WWW-Authenticate value for an unauthenticated MCP
// request. Pointing at the resource metadata is what starts a client's
// discovery, so a 401 without it strands the client (RFC 9728 §5.1).
func (p *Provider) ChallengeHeader(r *http.Request, reason string) string {
	h := fmt.Sprintf("Bearer resource_metadata=%q", p.baseURL(r)+ProtectedResourcePath)
	if reason != "" {
		h += fmt.Sprintf(`, error="invalid_token", error_description=%q`, reason)
	}
	return h
}

// ─── helpers ─────────────────────────────────────────────────────

func verifyPKCE(challenge, verifier string) bool {
	if challenge == "" || verifier == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

func hashClientID(clientID string) string {
	sum := sha256.Sum256([]byte(clientID))
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}

// sameResource compares resource identifiers, ignoring the differences RFC 8707
// says implementations should tolerate: case in scheme and host, and a trailing
// slash.
func sameResource(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSuffix(s, "/")
		u, err := url.Parse(s)
		if err != nil {
			return strings.ToLower(s)
		}
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		return u.String()
	}
	return norm(a) == norm(b)
}

func expired(issuedAt int64, ttl time.Duration) bool {
	return time.Now().After(time.Unix(issuedAt, 0).Add(ttl))
}

func slicesContain(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func displayName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "An MCP client"
	}
	return name
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(body)
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

// redirectError reports a failure to the client through its redirect URI, which
// is only safe once that URI has been verified against the registration.
func redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, description string) {
	target, err := url.Parse(redirectURI)
	if err != nil {
		renderError(w, http.StatusBadRequest, "Invalid redirect address", "The client's redirect address could not be parsed.")
		return
	}
	params := target.Query()
	params.Set("error", code)
	params.Set("error_description", description)
	if state != "" {
		params.Set("state", state)
	}
	target.RawQuery = params.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// ─── browser-facing pages ────────────────────────────────────────

type consentView struct {
	ClientName  string
	RedirectURI string
	Pending     string
	Action      string
}

var consentTemplate = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect your Google Health data</title>
<style>
 :root { color-scheme: light dark; }
 body { font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; margin: 0;
        display: grid; place-items: center; min-height: 100vh; padding: 1.5rem;
        background: Canvas; color: CanvasText; }
 main { max-width: 30rem; width: 100%; }
 h1 { font-size: 1.4rem; margin: 0 0 1rem; }
 dl { margin: 1.25rem 0; padding: 1rem; border: 1px solid color-mix(in srgb, CanvasText 25%, transparent);
      border-radius: 8px; }
 dt { font-size: .8rem; text-transform: uppercase; letter-spacing: .04em; opacity: .7; }
 dd { margin: .15rem 0 .85rem; word-break: break-all; }
 dd:last-child { margin-bottom: 0; }
 ul { padding-left: 1.2rem; }
 .row { display: flex; gap: .75rem; margin-top: 1.5rem; }
 button { font: inherit; padding: .6rem 1.1rem; border-radius: 8px; cursor: pointer; border: 1px solid transparent; }
 .primary { background: #1a73e8; color: #fff; }
 .secondary { background: transparent; color: inherit;
              border-color: color-mix(in srgb, CanvasText 35%, transparent); }
 .note { font-size: .85rem; opacity: .75; }
</style></head>
<body><main>
 <h1>Connect your Google Health data?</h1>
 <p><strong>{{.ClientName}}</strong> is asking to read your Google Health data through this server.</p>
 <dl>
  <dt>Requesting client</dt><dd>{{.ClientName}}</dd>
  <dt>You will be sent back to</dt><dd>{{.RedirectURI}}</dd>
 </dl>
 <p>If you continue, you will sign in at Google and choose what to share. This server can then:</p>
 <ul>
  <li>Read your health and fitness data — steps, heart rate, sleep, exercise, weight and similar</li>
  <li>Read your profile, settings and paired devices</li>
 </ul>
 <p class="note">It cannot add, change or delete anything. You can disconnect at any time at
  <a href="https://myaccount.google.com/permissions">myaccount.google.com/permissions</a>.</p>
 <p class="note"><strong>Only continue if you recognise the client above.</strong> If you did not
  start this, close this page.</p>
 <form method="post" action="{{.Action}}">
  <input type="hidden" name="pending" value="{{.Pending}}">
  <div class="row">
   <button class="primary" type="submit" name="approve" value="yes">Continue to Google</button>
   <button class="secondary" type="submit" name="approve" value="no">Cancel</button>
  </div>
 </form>
</main></body></html>`))

func renderConsent(w http.ResponseWriter, view consentView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The page carries a pending-authorization credential and must never be
	// framed by another site.
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	consentTemplate.Execute(w, view)
}

var errorTemplate = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
 :root { color-scheme: light dark; }
 body { font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; margin: 0;
        display: grid; place-items: center; min-height: 100vh; padding: 1.5rem;
        background: Canvas; color: CanvasText; }
 main { max-width: 30rem; }
 h1 { font-size: 1.3rem; margin: 0 0 .75rem; }
</style></head>
<body><main><h1>{{.Title}}</h1><p>{{.Detail}}</p></main></body></html>`))

func renderError(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	errorTemplate.Execute(w, struct{ Title, Detail string }{title, detail})
}
