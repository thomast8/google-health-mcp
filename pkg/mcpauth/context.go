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
	"fmt"
	"net/http"
)

type contextKey int

const (
	resolverKey contextKey = iota
	sessionKey
)

// TokenResolver yields a live Google access token for the caller of the current
// request.
//
// A resolver is passed down rather than a token because resolving may need a
// round trip to Google, and most MCP requests — initialize, tools/list, a
// rejected argument — never touch the Health API. Deferring the work means a
// session whose Google grant has been revoked still lists tools and reports a
// clear error only when data is actually requested.
type TokenResolver func(ctx context.Context) (string, error)

// WithSession attaches an authenticated session and its token resolver.
func WithSession(ctx context.Context, s *Session, resolve TokenResolver) context.Context {
	ctx = context.WithValue(ctx, sessionKey, s)
	return context.WithValue(ctx, resolverKey, resolve)
}

// SessionFromContext returns the authenticated session, if any.
func SessionFromContext(ctx context.Context) (*Session, bool) {
	s, ok := ctx.Value(sessionKey).(*Session)
	return s, ok && s != nil
}

// ResolveGoogleToken returns a live Google access token for the current request.
//
// It fails when no session is attached. That is the fail-closed guarantee for
// multi-tenant mode: a request that never authenticated cannot reach the Health
// API at all, rather than quietly falling back to whatever credentials the
// server process itself happens to hold.
func ResolveGoogleToken(ctx context.Context) (string, error) {
	resolve, ok := ctx.Value(resolverKey).(TokenResolver)
	if !ok || resolve == nil {
		return "", fmt.Errorf("no authenticated Google session for this request")
	}
	return resolve(ctx)
}

// Middleware authenticates an MCP request and attaches the session. An
// unauthenticated request gets a 401 carrying the discovery pointer that starts
// the client's OAuth flow.
func (p *Provider) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, err := p.Authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", p.ChallengeHeader(r, err.Error()))
			writeOAuthError(w, http.StatusUnauthorized, "invalid_token", err.Error())
			return
		}
		ctx := WithSession(r.Context(), session, func(ctx context.Context) (string, error) {
			return p.GoogleAccessToken(ctx, session)
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
