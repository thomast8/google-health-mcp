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

package output

import (
	"encoding/json"
	"strings"
	"testing"
)

// The generated exercise→heart-rate hint must use only API-supported
// comparators: >= and < (the API rejects <=).
func TestGenerateHints_ExerciseHRHintUsesSupportedComparators(t *testing.T) {
	data := json.RawMessage(`{
		"dataPoints": [
			{"start": "2026-03-29T14:18:32+01:00", "end": "2026-03-29T14:39:14+01:00", "exerciseType": "RUN"}
		]
	}`)

	hints := GenerateHints(data, HintRequest{DataType: "exercise", Operation: "list"})
	if len(hints) == 0 {
		t.Fatal("expected an exercise correlation hint")
	}
	for _, h := range hints {
		if strings.Contains(h, "<=") {
			t.Errorf("hint contains unsupported '<=': %s", h)
		}
	}
	found := false
	for _, h := range hints {
		if strings.Contains(h, `physical_time < "2026-03-29T13:39:14Z"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an upper bound using '<', got: %v", hints)
	}
}

// The sleep --detail suggestion must not fire when the caller already
// requested the detailed view.
func TestGenerateHints_SleepDetailHintSuppressedWhenDetailSet(t *testing.T) {
	data := json.RawMessage(`{
		"dataPoints": [
			{"start": "2026-06-11T00:42:00+01:00", "end": "2026-06-11T08:26:00+01:00", "stages": []}
		]
	}`)

	for _, h := range GenerateHints(data, HintRequest{DataType: "sleep", Operation: "list", Detail: true}) {
		if strings.Contains(h, "--detail") {
			t.Errorf("--detail hint fired even though detail was already set: %s", h)
		}
	}

	found := false
	for _, h := range GenerateHints(data, HintRequest{DataType: "sleep", Operation: "list"}) {
		if strings.Contains(h, "--detail") {
			found = true
		}
	}
	if !found {
		t.Error("expected the --detail hint when detail is not set")
	}
}

// Over MCP a hint has to name a tool call. The suggestion is the same either
// way, but a client with no shell cannot act on a command line, and a hint it
// cannot act on is worse than none.
func TestGenerateHints_MCPSurfaceNamesToolsNotCommandLines(t *testing.T) {
	cases := []struct {
		name string
		data string
		req  HintRequest
		want string // a substring the MCP phrasing must carry
	}{
		{
			name: "short daily-rollup suggests the list operation",
			data: `{"dataPoints":[{"date":"2026-08-17","countSum":1332}]}`,
			req:  HintRequest{DataType: "steps", Operation: "daily-rollup", From: "2026-08-17", To: "2026-08-17"},
			want: "call query_data with data_type 'steps', operation 'list', from '2026-08-17', to '2026-08-17'",
		},
		{
			name: "short daily-rollup of a sampled type suggests a limit",
			data: `{"dataPoints":[{"date":"2026-08-17","beatsPerMinuteAvg":61}]}`,
			req:  HintRequest{DataType: "heart-rate", Operation: "daily-rollup", From: "2026-08-17", To: "2026-08-17"},
			want: "limit 100",
		},
		{
			name: "sleep names the detail argument",
			data: `{"dataPoints":[{"start":"2026-06-11T00:42:00+01:00"}]}`,
			req:  HintRequest{DataType: "sleep", Operation: "list"},
			want: "detail: true",
		},
		{
			name: "exercise names the filter argument",
			data: `{"dataPoints":[{"start":"2026-03-29T14:18:32+01:00","end":"2026-03-29T14:39:14+01:00"}]}`,
			req:  HintRequest{DataType: "exercise", Operation: "list"},
			want: "call query_data with data_type 'heart-rate', operation 'list', filter '",
		},
		{
			name: "an empty range points at auth_status",
			data: `{"dataPoints":[]}`,
			req:  HintRequest{DataType: "weight", Operation: "list", From: "2026-08-01"},
			want: "call auth_status",
		},
		{
			name: "an empty page with more data names page_token",
			data: `{"dataPoints":[],"nextPageToken":"tok-9"}`,
			req:  HintRequest{DataType: "steps", Operation: "list", From: "2026-08-01"},
			want: "page_token 'tok-9'",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Surface = SurfaceMCP
			hints := GenerateHints(json.RawMessage(tc.data), tc.req)
			if len(hints) == 0 {
				t.Fatal("expected a hint")
			}
			joined := strings.Join(hints, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("MCP hint does not mention %q:\n%s", tc.want, joined)
			}
			for _, bad := range []string{"ghealth ", "--from", "--to", "--limit", "--detail", "--filter", "--page-token"} {
				if strings.Contains(joined, bad) {
					t.Errorf("MCP hint leaks CLI syntax %q:\n%s", bad, joined)
				}
			}
		})
	}
}

// The CLI surface is the zero value, so nothing that forgets to set a surface
// can accidentally serve tool-call wording to a shell user.
func TestGenerateHints_CLISurfaceIsTheDefault(t *testing.T) {
	data := json.RawMessage(`{"dataPoints":[{"date":"2026-08-17","countSum":1332}]}`)
	hints := GenerateHints(data, HintRequest{DataType: "steps", Operation: "daily-rollup", From: "2026-08-17", To: "2026-08-18"})
	if len(hints) == 0 {
		t.Fatal("expected a hint")
	}
	// The command line has to cover the range that was asked about, both ends.
	want := "ghealth data steps list --from 2026-08-17 --to 2026-08-18"
	if !strings.Contains(hints[0], want) {
		t.Errorf("hint %q does not carry the runnable command %q", hints[0], want)
	}
}

func TestTruncationHintPerSurface(t *testing.T) {
	if got := TruncationHint(SurfaceCLI, 500, "tok"); !strings.Contains(got, "--page-token tok") {
		t.Errorf("CLI truncation hint is not runnable: %s", got)
	}
	mcp := TruncationHint(SurfaceMCP, 500, "tok")
	if !strings.Contains(mcp, "page_token 'tok'") {
		t.Errorf("MCP truncation hint does not name page_token: %s", mcp)
	}
	if strings.Contains(mcp, "--") {
		t.Errorf("MCP truncation hint leaks CLI flags: %s", mcp)
	}
}

func TestSurfaceFromEnv(t *testing.T) {
	for value, want := range map[string]Surface{
		"":     SurfaceCLI,
		"cli":  SurfaceCLI,
		"mcp":  SurfaceMCP,
		"MCP":  SurfaceMCP,
		" mcp": SurfaceMCP,
	} {
		t.Setenv(SurfaceEnv, value)
		if got := SurfaceFromEnv(); got != want {
			t.Errorf("%s=%q gave surface %v, want %v", SurfaceEnv, value, got, want)
		}
	}
}
