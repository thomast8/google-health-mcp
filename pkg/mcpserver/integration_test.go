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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests drive the real `ghealth mcp` binary over stdio against a stub
// Health API. They are what proves the whole chain — MCP argument, command
// line, HTTP call, simplification, hints, envelope — actually joins up; the
// unit tests above only check each link in isolation.

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// ghealthBinary builds the CLI once per test run.
func ghealthBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping: builds the ghealth binary")
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ghealth-mcp-test")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "ghealth")
		cmd := exec.Command("go", "build", "-o", binPath, "../..")
		cmd.Stderr = os.Stderr
		buildErr = cmd.Run()
	})
	if buildErr != nil {
		t.Fatalf("build ghealth: %v", buildErr)
	}
	return binPath
}

// stubAPI stands in for the Health API, recording the requests it receives.
type stubAPI struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
}

func newStubAPI(t *testing.T, routes map[string]string) *stubAPI {
	t.Helper()
	s := &stubAPI{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Clone(context.Background()))
		s.mu.Unlock()

		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"code":404,"message":"no stub for ` + r.URL.Path + `"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stubAPI) lastRequest(t *testing.T) *http.Request {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("the stub API was never called")
	}
	return s.requests[len(s.requests)-1]
}

// connect starts `ghealth mcp` over stdio and returns a connected client.
func connect(t *testing.T, api *stubAPI) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	cmd := exec.Command(ghealthBinary(t), "mcp")
	cmd.Env = append(os.Environ(),
		"GHEALTH_BASE_URL="+api.URL,
		"GHEALTH_ACCESS_TOKEN=test-token",
		// An isolated, empty config dir: the test must never read the
		// developer's real credentials, nor inherit their default format.
		"GHEALTH_CONFIG_DIR="+t.TempDir(),
		"GHEALTH_FORMAT=",
		"GHEALTH_PROFILE=",
	)
	cmd.Stderr = os.Stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to ghealth mcp: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func callTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			t.Fatalf("expected TextContent, got %T", c)
		}
		b.WriteString(tc.Text)
	}
	return b.String()
}

const stepsListResponse = `{
  "dataPoints": [
    {
      "name": "users/me/dataTypes/steps/dataPoints/dp1",
      "interval": {
        "startTime": "2026-03-22T08:00:00Z",
        "endTime": "2026-03-22T09:00:00Z",
        "civilStartTime": "2026-03-22T08:00:00",
        "civilEndTime": "2026-03-22T09:00:00"
      },
      "steps": {"count": "1234"}
    }
  ]
}`

const stepsDailyRollupResponse = `{
  "rollupDataPoints": [
    {"startTime": "2026-03-22T00:00:00Z", "endTime": "2026-03-23T00:00:00Z", "steps": {"countSum": "9037"}}
  ]
}`

// The whole chain, end to end: an MCP tool call becomes a Health API request
// and the API's response comes back as the tool result.
func TestEndToEndQueryData(t *testing.T) {
	api := newStubAPI(t, map[string]string{
		"/users/me/dataTypes/steps/dataPoints": stepsListResponse,
	})
	session := connect(t, api)

	res := callTool(t, session, "query_data", map[string]any{
		"data_type": "steps",
		"operation": "list",
		"from":      "2026-03-22",
		"to":        "2026-03-23",
		"limit":     10,
	})
	if res.IsError {
		t.Fatalf("tool reported an error: %s", text(t, res))
	}

	var payload struct {
		DataPoints []map[string]any `json:"dataPoints"`
	}
	if err := json.Unmarshal([]byte(text(t, res)), &payload); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text(t, res))
	}
	if len(payload.DataPoints) != 1 {
		t.Fatalf("got %d data points, want 1:\n%s", len(payload.DataPoints), text(t, res))
	}

	// The from/to arguments must reach the API as a filter expression built for
	// this type's time field, not be silently dropped.
	filter := api.lastRequest(t).URL.Query().Get("filter")
	if !strings.Contains(filter, "steps.interval.civil_start_time") {
		t.Errorf("filter %q does not constrain the steps interval", filter)
	}
	if !strings.Contains(filter, "2026-03-22") {
		t.Errorf("filter %q does not carry the requested start date", filter)
	}
}

// daily-rollup is the operation the instructions steer agents towards, so it
// has to work over MCP, POST a rollup body, and come back in the stable
// dataPoints envelope.
func TestEndToEndDailyRollup(t *testing.T) {
	api := newStubAPI(t, map[string]string{
		"/users/me/dataTypes/steps/dataPoints:dailyRollUp": stepsDailyRollupResponse,
	})
	session := connect(t, api)

	res := callTool(t, session, "query_data", map[string]any{
		"data_type": "steps",
		"operation": "daily-rollup",
		"from":      "2026-03-22",
		"to":        "2026-03-23",
	})
	if res.IsError {
		t.Fatalf("tool reported an error: %s", text(t, res))
	}

	var payload struct {
		DataPoints []map[string]any `json:"dataPoints"`
	}
	if err := json.Unmarshal([]byte(text(t, res)), &payload); err != nil {
		t.Fatalf("result is not the standard envelope: %v\n%s", err, text(t, res))
	}
	if len(payload.DataPoints) != 1 {
		t.Fatalf("got %d rollup buckets, want 1:\n%s", len(payload.DataPoints), text(t, res))
	}
	if req := api.lastRequest(t); req.Method != http.MethodPost {
		t.Errorf("rollup used %s, want POST", req.Method)
	}
}

// An API failure has to arrive as a tool error carrying the CLI's message,
// so the agent can tell a bad request from a broken server.
func TestEndToEndAPIErrorBecomesAToolError(t *testing.T) {
	api := newStubAPI(t, nil) // every path 404s
	session := connect(t, api)

	res := callTool(t, session, "query_data", map[string]any{
		"data_type": "steps",
		"operation": "list",
	})
	if !res.IsError {
		t.Fatalf("expected a tool error, got:\n%s", text(t, res))
	}
	if got := text(t, res); !strings.Contains(got, "404") && !strings.Contains(strings.ToLower(got), "not found") {
		t.Errorf("error text does not explain the failure:\n%s", got)
	}
}

// list_data_types answers from the registry, so it must work with no API and
// no credentials at all — it is the entry point an agent reaches for first.
func TestEndToEndListDataTypesNeedsNoAPI(t *testing.T) {
	api := newStubAPI(t, nil)
	session := connect(t, api)

	res := callTool(t, session, "list_data_types", map[string]any{"category": "sleep"})
	if res.IsError {
		t.Fatalf("tool reported an error: %s", text(t, res))
	}

	var payload struct {
		Count     int `json:"count"`
		DataTypes []struct {
			ID string `json:"id"`
		} `json:"dataTypes"`
	}
	if err := json.Unmarshal([]byte(text(t, res)), &payload); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if payload.Count == 0 {
		t.Fatal("no sleep types returned")
	}
}

// The server's instructions are how a client learns the daily-rollup and
// pagination conventions before it makes its first call.
func TestEndToEndServerAdvertisesInstructionsAndTools(t *testing.T) {
	api := newStubAPI(t, nil)
	session := connect(t, api)

	if got := session.InitializeResult().Instructions; !strings.Contains(got, "daily-rollup") {
		t.Errorf("instructions do not mention daily-rollup:\n%s", got)
	}

	ctx := context.Background()
	var names []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		names = append(names, tool.Name)
	}
	if len(names) != 6 {
		t.Errorf("advertised %d tools, want 6: %v", len(names), names)
	}
}
