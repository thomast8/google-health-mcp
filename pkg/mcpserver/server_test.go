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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToken = "s3cr3t-token"

// HTTP mode must fail closed. A deployment gets a public URL as soon as it
// boots, so starting without a token would publish the user's health record.
func TestHandlerRefusesToServeWithoutAToken(t *testing.T) {
	for _, token := range []string{"", "   "} {
		h, err := Handler(New(Options{Runner: &recordingRunner{}}), token)
		if !errors.Is(err, ErrNoToken) {
			t.Errorf("token %q: got (%v, %v), want ErrNoToken", token, h, err)
		}
		if h != nil {
			t.Errorf("token %q: a handler was returned despite the error", token)
		}
	}
}

func TestMCPEndpointRequiresTheBearerToken(t *testing.T) {
	srv := newTestHTTPServer(t)

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong", http.StatusUnauthorized},
		{"token without the Bearer prefix", testToken, http.StatusUnauthorized},
		{"wrong scheme", "Basic " + testToken, http.StatusUnauthorized},
		{"token as a prefix of the real one", "Bearer s3cr3t", http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, srv.URL+MCPPath, strings.NewReader(initializeRequest))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			resp := doRequest(t, srv, req)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("a 401 should carry WWW-Authenticate so a connector knows to supply credentials")
			}
		})
	}
}

func TestMCPEndpointAcceptsTheBearerToken(t *testing.T) {
	srv := newTestHTTPServer(t)

	req := httptest.NewRequest(http.MethodPost, srv.URL+MCPPath, strings.NewReader(initializeRequest))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+testToken)

	resp := doRequest(t, srv, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
}

// The health check has to answer without credentials, or the host's probe
// marks a working deployment as unhealthy.
func TestHealthEndpointIsUnauthenticated(t *testing.T) {
	srv := newTestHTTPServer(t)

	req := httptest.NewRequest(http.MethodGet, srv.URL+HealthPath, nil)
	resp := doRequest(t, srv, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
}

func newTestHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	handler, err := Handler(New(Options{Runner: &recordingRunner{}, Version: "test"}), testToken)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func doRequest(t *testing.T, srv *httptest.Server, req *http.Request) *http.Response {
	t.Helper()
	// httptest.NewRequest builds a server-side request; reissue it as a client
	// request against the live test server.
	out, err := http.NewRequestWithContext(context.Background(), req.Method, req.URL.String(), req.Body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	out.Header = req.Header
	resp, err := srv.Client().Do(out)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

const initializeRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`

// Every tool must be reachable and described, since the description is all a
// model has to choose between them.
func TestServerAdvertisesEveryTool(t *testing.T) {
	ctx := context.Background()
	srv := New(Options{Runner: &recordingRunner{}, Version: "test"})

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	found := map[string]bool{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %s has no input schema", tool.Name)
		}
		found[tool.Name] = true
	}

	for _, name := range []string{
		"list_data_types", "describe_data_type", "query_data",
		"get_user_info", "auth_status", "export_exercise_tcx",
	} {
		if !found[name] {
			t.Errorf("tool %s is not advertised", name)
		}
	}
	if len(found) != 6 {
		t.Errorf("advertised %d tools, want 6: %v", len(found), sortedKeys(found))
	}
}

// A tool error must reach the client as a tool error carrying the CLI's own
// message, not as a transport failure.
func TestToolErrorsReachTheClient(t *testing.T) {
	ctx := context.Background()
	srv := New(Options{Runner: &recordingRunner{}, Version: "test"})

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "query_data",
		Arguments: map[string]any{"data_type": "steps", "operation": "delete"},
	})
	if err != nil {
		t.Fatalf("CallTool returned a protocol error, want a tool error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError to be set")
	}
	if got := resultText(t, res); !strings.Contains(got, "read-only") {
		t.Errorf("error text %q does not explain the refusal", got)
	}
}
