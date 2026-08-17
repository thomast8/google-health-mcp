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
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// jsonTools are the tools that answer in JSON and must therefore declare an
// output schema. export_exercise_tcx is absent on purpose: it returns CSV rows
// or TCX XML, and there is no JSON shape to describe.
var jsonTools = map[string]bool{
	"list_data_types":    true,
	"describe_data_type": true,
	"query_data":         true,
	"get_user_info":      true,
	"auth_status":        true,
}

func connectInMemory(t *testing.T, r Runner) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := New(Options{Runner: r, Version: "test"})

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// A declared output schema is what lets a client see the shape of an answer
// before spending a call on it, so every JSON tool must carry one — and it must
// describe an object, since that is what the tools return.
func TestToolsAdvertiseOutputSchemas(t *testing.T) {
	ctx := context.Background()
	cs := connectInMemory(t, &recordingRunner{})

	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		if !jsonTools[tool.Name] {
			if tool.OutputSchema != nil {
				t.Errorf("tool %s declares an output schema but does not answer in JSON", tool.Name)
			}
			continue
		}
		if tool.OutputSchema == nil {
			t.Errorf("tool %s has no output schema", tool.Name)
			continue
		}
		schema, ok := tool.OutputSchema.(*jsonschema.Schema)
		if !ok {
			// Over the wire the schema arrives as generic JSON; decode it.
			raw, mErr := json.Marshal(tool.OutputSchema)
			if mErr != nil {
				t.Errorf("tool %s: output schema does not marshal: %v", tool.Name, mErr)
				continue
			}
			schema = &jsonschema.Schema{}
			if uErr := json.Unmarshal(raw, schema); uErr != nil {
				t.Errorf("tool %s: output schema is not a JSON Schema: %v", tool.Name, uErr)
				continue
			}
		}
		if schema.Type != "object" {
			t.Errorf("tool %s: output schema has type %q, want object", tool.Name, schema.Type)
		}
		if schema.Description == "" {
			t.Errorf("tool %s: output schema has no description", tool.Name)
		}
	}
}

// Every output schema has to resolve, or the SDK would reject it at registration
// and the failure would only show up when a client connected.
func TestOutputSchemasResolve(t *testing.T) {
	for name, schema := range map[string]*jsonschema.Schema{
		"query_data":         queryDataOutputSchema,
		"list_data_types":    listDataTypesOutputSchema,
		"describe_data_type": describeDataTypeOutputSchema,
		"auth_status":        authStatusOutputSchema,
		"get_user_info":      getUserInfoOutputSchema,
	} {
		if _, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true}); err != nil {
			t.Errorf("%s output schema does not resolve: %v", name, err)
		}
	}
}

// A tool result must carry both halves: structured content for a client that
// reads it, and the CLI's own bytes as text for one that does not. They have to
// agree, since an agent may read either.
func TestStructuredContentMatchesTheTextBlock(t *testing.T) {
	ctx := context.Background()
	payload := "{\n  \"dataPoints\": [\n    {\n      \"date\": \"2026-08-17\",\n      \"countSum\": 1332\n    }\n  ]\n}\n"
	cs := connectInMemory(t, &recordingRunner{stdout: []byte(payload)})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "query_data",
		Arguments: map[string]any{"data_type": "steps", "operation": "daily-rollup", "from": "today", "to": "today"},
	})
	if err != nil {
		t.Fatalf("call query_data: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool reported an error: %s", resultText(t, res))
	}

	// The text block is the CLI's stdout, unaltered — indentation and key order
	// included. That is the contract the CLI-as-child design rests on.
	if got := resultText(t, res); got != payload {
		t.Errorf("text block was altered\n got: %q\nwant: %q", got, payload)
	}

	if res.StructuredContent == nil {
		t.Fatal("no structured content — the declared output schema promises some")
	}
	var structured, fromText any
	if err := json.Unmarshal(mustJSON(t, res.StructuredContent), &structured); err != nil {
		t.Fatalf("structured content is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(payload), &fromText); err != nil {
		t.Fatalf("text block is not JSON: %v", err)
	}
	if mustCanonical(t, structured) != mustCanonical(t, fromText) {
		t.Errorf("structured content and text block disagree\n structured: %s\n text:       %s",
			mustCanonical(t, structured), mustCanonical(t, fromText))
	}
}

// Output that does not parse as JSON must still reach the caller. A
// `--format json` command printing something else is a bug worth seeing, not a
// reason to fail the call and hide it.
func TestNonJSONOutputStillReachesTheCaller(t *testing.T) {
	ctx := context.Background()
	cs := connectInMemory(t, &recordingRunner{stdout: []byte("not json at all")})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "query_data",
		Arguments: map[string]any{"data_type": "steps", "operation": "list"},
	})
	if err != nil {
		t.Fatalf("call query_data: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool reported an error: %s", resultText(t, res))
	}
	if got := resultText(t, res); got != "not json at all" {
		t.Errorf("output was not relayed: %q", got)
	}
	if res.StructuredContent != nil {
		t.Errorf("structured content was invented from non-JSON output: %s", res.StructuredContent)
	}
}

// `raw: true` returns the untouched Health API response, whose shape this server
// does not control. The output schema must accept it rather than turning a
// working read into a validation error.
func TestRawResponsesValidateAgainstTheOutputSchema(t *testing.T) {
	ctx := context.Background()
	raw := `{"dataPoints":[{"name":"users/me/dataTypes/steps/dataPoints/dp1","steps":{"count":"1332","interval":{"startTime":"2026-08-17T07:00:00Z"}},"dataSource":{"platform":"ANDROID"}}],"nextPageToken":"tok"}`
	cs := connectInMemory(t, &recordingRunner{stdout: []byte(raw)})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "query_data",
		Arguments: map[string]any{"data_type": "steps", "operation": "list", "raw": true},
	})
	if err != nil {
		t.Fatalf("call query_data: %v", err)
	}
	if res.IsError {
		t.Fatalf("a raw response failed output validation: %s", resultText(t, res))
	}
	if res.StructuredContent == nil {
		t.Error("no structured content for a raw response")
	}
}

// Both auth modes mark their children as serving MCP, so the hints the CLI
// generates name tool calls instead of command lines the client cannot run.
func TestChildProcessesAreMarkedAsServingMCP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runner  *ExecRunner
		context context.Context
	}{
		{"single-account", &ExecRunner{}, context.Background()},
		{"multi-user", &ExecRunner{PerRequestAuth: true}, sessionCtx("tok")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := printEnv(t, tc.runner, tc.context, "GHEALTH_SURFACE")
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if strings.TrimSpace(got) != "mcp" {
				t.Errorf("GHEALTH_SURFACE is %q, want mcp", strings.TrimSpace(got))
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// mustCanonical re-encodes a decoded value so two JSON documents can be compared
// without indentation or key order getting in the way.
func mustCanonical(t *testing.T, v any) string {
	t.Helper()
	return string(mustJSON(t, v))
}

// The annotations are how a client decides whether a call is safe to make or to
// retry. Every tool here reads, so all of them must say so.
func TestEveryToolIsAnnotatedAsAReadOnlyRetryableCall(t *testing.T) {
	ctx := context.Background()
	cs := connectInMemory(t, &recordingRunner{})

	seen := 0
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		seen++
		a := tool.Annotations
		if a == nil {
			t.Errorf("tool %s carries no annotations", tool.Name)
			continue
		}
		if !a.ReadOnlyHint {
			t.Errorf("tool %s is not annotated read-only, though this server cannot write", tool.Name)
		}
		if !a.IdempotentHint {
			t.Errorf("tool %s is not annotated idempotent, though repeating a read changes nothing", tool.Name)
		}
		if a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Errorf("tool %s is not annotated open-world, though it reads Google's servers", tool.Name)
		}
	}
	if seen == 0 {
		t.Fatal("no tools were advertised")
	}
}
