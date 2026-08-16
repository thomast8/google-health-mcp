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

	"ghealth/pkg/types"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// recordingRunner captures the command line a tool builds and returns a canned
// payload, so a test can assert on argv without touching the network.
type recordingRunner struct {
	calls  [][]string
	stdout []byte
	err    error
}

func (r *recordingRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	if r.err != nil {
		return nil, r.err
	}
	if r.stdout == nil {
		return []byte(`{"dataPoints":[]}`), nil
	}
	return r.stdout, nil
}

func (r *recordingRunner) lastCall(t *testing.T) []string {
	t.Helper()
	if len(r.calls) == 0 {
		t.Fatal("runner was never invoked")
	}
	return r.calls[len(r.calls)-1]
}

func TestBuildQueryArgs(t *testing.T) {
	tests := []struct {
		name string
		in   queryDataInput
		want []string
	}{
		{
			name: "list with range and limit",
			in:   queryDataInput{DataType: "steps", Operation: "list", From: "2026-03-22", To: "2026-03-29", Limit: 10},
			want: []string{"data", "steps", "list", "--from", "2026-03-22", "--to", "2026-03-29", "--limit", "10", "--format", "json"},
		},
		{
			name: "list omits a zero limit so the CLI default applies",
			in:   queryDataInput{DataType: "steps", Operation: "list"},
			want: []string{"data", "steps", "list", "--format", "json"},
		},
		{
			name: "list resumes from a page token",
			in:   queryDataInput{DataType: "steps", Operation: "list", PageToken: "tok-1"},
			want: []string{"data", "steps", "list", "--page-token", "tok-1", "--format", "json"},
		},
		{
			name: "raw filter passes through",
			in:   queryDataInput{DataType: "steps", Operation: "list", Filter: `steps.interval.civil_start_time >= "2026-03-01"`},
			want: []string{"data", "steps", "list", "--filter", `steps.interval.civil_start_time >= "2026-03-01"`, "--format", "json"},
		},
		{
			name: "sleep list accepts detail",
			in:   queryDataInput{DataType: "sleep", Operation: "list", Detail: true},
			want: []string{"data", "sleep", "list", "--detail", "--format", "json"},
		},
		{
			name: "get requires only the id",
			in:   queryDataInput{DataType: "exercise", Operation: "get", ID: "abc123"},
			want: []string{"data", "exercise", "get", "--id", "abc123", "--format", "json"},
		},
		{
			name: "daily-rollup with an explicit window",
			in:   queryDataInput{DataType: "steps", Operation: "daily-rollup", From: "2026-03-01", To: "2026-03-08", WindowDays: 7},
			want: []string{"data", "steps", "daily-rollup", "--from", "2026-03-01", "--to", "2026-03-08", "--window-days", "7", "--format", "json"},
		},
		{
			name: "rollup with an explicit window size",
			in:   queryDataInput{DataType: "steps", Operation: "rollup", From: "2026-03-01", To: "2026-03-02", WindowSize: "3600s"},
			want: []string{"data", "steps", "rollup", "--from", "2026-03-01", "--to", "2026-03-02", "--window-size", "3600s", "--format", "json"},
		},
		{
			name: "raw is forwarded",
			in:   queryDataInput{DataType: "steps", Operation: "list", Raw: true},
			want: []string{"data", "steps", "list", "--raw", "--format", "json"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildQueryArgs(tc.in)
			if err != nil {
				t.Fatalf("buildQueryArgs: %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("argv mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestBuildQueryArgsRejects(t *testing.T) {
	tests := []struct {
		name    string
		in      queryDataInput
		wantSub string
	}{
		{
			name:    "unknown type",
			in:      queryDataInput{DataType: "sleeping", Operation: "list"},
			wantSub: "unknown data type",
		},
		{
			name:    "missing type",
			in:      queryDataInput{Operation: "list"},
			wantSub: "data_type is required",
		},
		{
			name:    "missing operation",
			in:      queryDataInput{DataType: "steps"},
			wantSub: "operation is required",
		},
		{
			// The registry marks exercise writable; the MCP surface still must not.
			name:    "write operation is refused even where the type supports it",
			in:      queryDataInput{DataType: "exercise", Operation: "delete", ID: "abc"},
			wantSub: "read-only",
		},
		{
			name:    "operation the type does not support",
			in:      queryDataInput{DataType: "floors", Operation: "list"},
			wantSub: "does not support",
		},
		{
			name:    "get without an id",
			in:      queryDataInput{DataType: "exercise", Operation: "get"},
			wantSub: "id is required",
		},
		{
			name:    "rollup without a range",
			in:      queryDataInput{DataType: "steps", Operation: "rollup", From: "2026-03-01"},
			wantSub: "from and to are both required",
		},
		{
			name:    "daily-rollup without a range",
			in:      queryDataInput{DataType: "steps", Operation: "daily-rollup"},
			wantSub: "from and to are both required",
		},
		{
			name:    "detail on a type that has no such flag",
			in:      queryDataInput{DataType: "steps", Operation: "list", Detail: true},
			wantSub: "only supported for sleep",
		},
		{
			name:    "negative limit",
			in:      queryDataInput{DataType: "steps", Operation: "list", Limit: -1},
			wantSub: "must not be negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildQueryArgs(tc.in)
			if err == nil {
				t.Fatalf("expected an error, got argv %q", got)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// A rollup range error should name the cap the API actually enforces for that
// type, since the two caps differ and the caller has to split the range.
func TestRequireRangeReportsThePerTypeCap(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"heart-rate", "14 days"},
		{"steps", "90 days"},
	} {
		err := requireRange("", "", tc.id, "rollup")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: expected the message to mention %q, got %v", tc.id, tc.want, err)
		}
	}
}

func TestResolveDataTypeSuggestsNearMisses(t *testing.T) {
	// Underscores are the most likely slip, since the API's own filter names
	// use them while the CLI's type IDs are hyphenated.
	if _, err := resolveDataType("heart_rate"); err == nil || !strings.Contains(err.Error(), "heart-rate") {
		t.Errorf("expected a heart-rate suggestion, got %v", err)
	}
	if _, err := resolveDataType("heart"); err == nil || !strings.Contains(err.Error(), "did you mean") {
		t.Errorf("expected suggestions for a partial name, got %v", err)
	}
	if id, err := resolveDataType("  steps  "); err != nil || id != "steps" {
		t.Errorf("expected surrounding space to be tolerated, got %q / %v", id, err)
	}
}

func TestListDataTypesFiltersByCategory(t *testing.T) {
	res, _, err := listDataTypes(context.Background(), nil, listDataTypesInput{Category: "sleep"})
	if err != nil {
		t.Fatalf("listDataTypes: %v", err)
	}

	var payload struct {
		Count     int    `json:"count"`
		Category  string `json:"category"`
		DataTypes []struct {
			ID       string `json:"id"`
			Category string `json:"category"`
		} `json:"dataTypes"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}

	if payload.Count == 0 || payload.Count != len(payload.DataTypes) {
		t.Fatalf("count %d does not match %d returned types", payload.Count, len(payload.DataTypes))
	}
	if payload.Count == len(types.Registry) {
		t.Error("the category filter returned the whole registry")
	}
	for _, dt := range payload.DataTypes {
		if dt.Category != "sleep" {
			t.Errorf("type %s has category %s, want sleep", dt.ID, dt.Category)
		}
	}
}

func TestListDataTypesRejectsUnknownCategory(t *testing.T) {
	_, _, err := listDataTypes(context.Background(), nil, listDataTypesInput{Category: "cardio"})
	if err == nil || !strings.Contains(err.Error(), "known categories are") {
		t.Fatalf("expected the error to list the known categories, got %v", err)
	}
}

func TestListDataTypesWithoutFilterCoversTheRegistry(t *testing.T) {
	res, _, err := listDataTypes(context.Background(), nil, listDataTypesInput{})
	if err != nil {
		t.Fatalf("listDataTypes: %v", err)
	}
	var payload struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if payload.Count != len(types.Registry) {
		t.Errorf("returned %d types, want the full registry of %d", payload.Count, len(types.Registry))
	}
}

func TestGetUserInfoMapsResources(t *testing.T) {
	for resource, want := range map[string][]string{
		"identity":       {"user", "identity", "--format", "json"},
		"profile":        {"user", "profile", "get", "--format", "json"},
		"settings":       {"user", "settings", "get", "--format", "json"},
		"irn-profile":    {"user", "irn-profile", "--format", "json"},
		"paired-devices": {"user", "paired-devices", "list", "--format", "json"},
	} {
		r := &recordingRunner{}
		if _, _, err := getUserInfo(r)(context.Background(), nil, getUserInfoInput{Resource: resource}); err != nil {
			t.Fatalf("%s: %v", resource, err)
		}
		if got := r.lastCall(t); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: argv %q, want %q", resource, got, want)
		}
	}
}

func TestGetUserInfoRejectsUnknownResource(t *testing.T) {
	r := &recordingRunner{}
	_, _, err := getUserInfo(r)(context.Background(), nil, getUserInfoInput{Resource: "devices"})
	if err == nil || !strings.Contains(err.Error(), "paired-devices") {
		t.Fatalf("expected the error to list valid resources, got %v", err)
	}
	if len(r.calls) != 0 {
		t.Error("an invalid resource still reached the CLI")
	}
}

func TestExportExerciseTCX(t *testing.T) {
	t.Run("defaults to csv and writes to stdout", func(t *testing.T) {
		r := &recordingRunner{stdout: []byte("time,lap\n")}
		if _, _, err := exportExerciseTCX(r)(context.Background(), nil, exportExerciseTCXInput{ID: "e1"}); err != nil {
			t.Fatalf("export: %v", err)
		}
		want := "data exercise export-tcx --id e1 --output - --as csv"
		if got := strings.Join(r.lastCall(t), " "); got != want {
			t.Errorf("argv %q, want %q", got, want)
		}
	})

	t.Run("honours tcx", func(t *testing.T) {
		r := &recordingRunner{stdout: []byte("<xml/>")}
		if _, _, err := exportExerciseTCX(r)(context.Background(), nil, exportExerciseTCXInput{ID: "e1", As: "tcx"}); err != nil {
			t.Fatalf("export: %v", err)
		}
		if got := strings.Join(r.lastCall(t), " "); !strings.HasSuffix(got, "--as tcx") {
			t.Errorf("argv %q does not end with --as tcx", got)
		}
	})

	t.Run("rejects an unknown format and a missing id", func(t *testing.T) {
		r := &recordingRunner{}
		if _, _, err := exportExerciseTCX(r)(context.Background(), nil, exportExerciseTCXInput{ID: "e1", As: "gpx"}); err == nil {
			t.Error("expected --as gpx to be rejected")
		}
		if _, _, err := exportExerciseTCX(r)(context.Background(), nil, exportExerciseTCXInput{As: "csv"}); err == nil {
			t.Error("expected a missing id to be rejected")
		}
		if len(r.calls) != 0 {
			t.Error("an invalid request still reached the CLI")
		}
	})
}

func TestDescribeDataTypeValidatesBeforeRunning(t *testing.T) {
	r := &recordingRunner{}
	if _, _, err := describeDataType(r)(context.Background(), nil, describeDataTypeInput{DataType: "nonsense"}); err == nil {
		t.Fatal("expected an unknown type to be rejected")
	}
	if len(r.calls) != 0 {
		t.Error("an unknown type still reached the CLI")
	}

	if _, _, err := describeDataType(r)(context.Background(), nil, describeDataTypeInput{DataType: "steps"}); err != nil {
		t.Fatalf("describe steps: %v", err)
	}
	want := "schema type steps --format json"
	if got := strings.Join(r.lastCall(t), " "); got != want {
		t.Errorf("argv %q, want %q", got, want)
	}
}

func TestAuthStatus(t *testing.T) {
	r := &recordingRunner{stdout: []byte(`{"authenticated":true}`)}
	if _, _, err := authStatus(r)(context.Background(), nil, authStatusInput{}); err != nil {
		t.Fatalf("authStatus: %v", err)
	}
	if got := strings.Join(r.lastCall(t), " "); got != "auth status --format json" {
		t.Errorf("argv %q", got)
	}
}

// Tool output must reach the client untouched: the CLI's _hints and
// nextPageToken are what let an agent make the next call.
func TestQueryDataReturnsCLIOutputVerbatim(t *testing.T) {
	payload := `{"dataPoints":[{"steps":42}],"_hints":["try daily-rollup"],"nextPageToken":"t2"}`
	r := &recordingRunner{stdout: []byte(payload)}
	res, _, err := queryData(r)(context.Background(), nil, queryDataInput{DataType: "steps", Operation: "list"})
	if err != nil {
		t.Fatalf("queryData: %v", err)
	}
	if got := resultText(t, res); got != payload {
		t.Errorf("output was altered\n got: %s\nwant: %s", got, payload)
	}
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("expected exactly one content block, got %+v", res)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	return text.Text
}
