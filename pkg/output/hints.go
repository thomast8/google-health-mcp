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
	"fmt"
	"os"
	"strings"
	"time"
)

// Surface identifies the caller a hint is written for.
//
// A hint is a next-step suggestion, so it has to name a step the reader can
// actually take. An MCP client has no shell: telling it to run
// "ghealth data sleep list --detail" is worse than saying nothing, because the
// suggestion is real but the mechanism is not. The same suggestion is therefore
// rendered as a command line for the CLI and as a tool call for MCP.
type Surface int

const (
	// SurfaceCLI phrases hints as ghealth command lines. It is the zero value,
	// so anything that does not opt in gets the CLI wording.
	SurfaceCLI Surface = iota
	// SurfaceMCP phrases hints as MCP tool calls, naming the tool and its
	// argument names.
	SurfaceMCP
)

// SurfaceEnv is the environment variable that selects the hint surface. The MCP
// server sets it to "mcp" on the CLI child processes it runs; nothing else sets
// it, so the CLI's own users always get command lines.
//
// It travels in the environment rather than in argv deliberately: the MCP layer
// asserts the exact argv it builds, and hint phrasing is not part of the request
// being made.
const SurfaceEnv = "GHEALTH_SURFACE"

// SurfaceFromEnv reads the surface this process is generating output for.
func SurfaceFromEnv() Surface {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(SurfaceEnv)), "mcp") {
		return SurfaceMCP
	}
	return SurfaceCLI
}

// HintRequest describes the call whose response is being annotated. It carries
// the parameters the hint text needs to name a concrete follow-up — the type and
// operation that were asked for, the range they covered, and the surface the
// answer is going to.
type HintRequest struct {
	DataType  string
	Operation string
	From      string
	To        string
	// Detail reports whether the caller already asked for the detailed sleep
	// view, so the hint suggesting it is not emitted redundantly.
	Detail  bool
	Surface Surface
}

// GenerateHints produces contextual hints from a response, the request that
// produced it, and the surface that will read them. Hints help a caller make a
// better next call without being opinionated about the task.
func GenerateHints(data json.RawMessage, req HintRequest) []string {
	var hints []string

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil
	}

	// Count data points.
	var dataPoints []json.RawMessage
	if raw, ok := obj["dataPoints"]; ok {
		json.Unmarshal(raw, &dataPoints)
	}
	var rollupPoints []json.RawMessage
	if raw, ok := obj["rollupDataPoints"]; ok {
		json.Unmarshal(raw, &rollupPoints)
	}

	nPoints := len(dataPoints) + len(rollupPoints)

	// ── Hint 1: Wrong resolution ─────────────────────────────────

	if req.Operation == "daily-rollup" && nPoints > 0 && nPoints <= 3 {
		// Short range daily rollup — suggest list for more detail.
		switch req.DataType {
		case "heart-rate", "oxygen-saturation", "heart-rate-variability":
			hints = append(hints, req.Surface.pick(
				fmt.Sprintf("For %d days of data, '%s' gives individual readings with timestamps.",
					nPoints, cliList(req.DataType, req.From, req.To, " --limit 100")),
				fmt.Sprintf("For individual readings with timestamps across these %d days, %s.",
					nPoints, toolList(req.DataType, req.From, req.To, ", limit 100"))))
		case "steps", "distance", "swim-lengths-data":
			hints = append(hints, req.Surface.pick(
				fmt.Sprintf("For detailed per-interval data, '%s' shows individual records.",
					cliList(req.DataType, req.From, req.To, "")),
				fmt.Sprintf("For detailed per-interval records, %s.",
					toolList(req.DataType, req.From, req.To, ""))))
		}
	}

	// ── Hint 2: Sleep detail ─────────────────────────────────────

	if req.Operation == "list" && len(dataPoints) > 0 {
		// Sleep without the per-stage breakdown.
		if req.DataType == "sleep" && !req.Detail {
			hints = append(hints, req.Surface.pick(
				"Add --detail for per-stage sleep breakdown (AWAKE, LIGHT, DEEP, REM timestamps).",
				"Set detail: true for the per-stage sleep breakdown (AWAKE, LIGHT, DEEP, REM timestamps)."))
		}
	}

	// ── Hint 3: Related data ─────────────────────────────────────

	if req.Operation == "list" && req.DataType == "exercise" && len(dataPoints) > 0 {
		// Parse first exercise to suggest HR correlation.
		var dp map[string]interface{}
		json.Unmarshal(dataPoints[0], &dp)
		if start, ok := dp["start"].(string); ok {
			if end, ok := dp["end"].(string); ok {
				filter := fmt.Sprintf(
					`heart_rate.sample_time.physical_time >= "%s" AND heart_rate.sample_time.physical_time < "%s"`,
					toUTC(start), toUTC(end))
				hints = append(hints, req.Surface.pick(
					fmt.Sprintf("For heart rate during this exercise, use: ghealth data heart-rate list --filter '%s'", filter),
					fmt.Sprintf("For heart rate during this exercise, call query_data with data_type 'heart-rate', operation 'list', filter '%s'.", filter)))
			}
		}
	}

	if req.Operation == "list" && req.DataType == "sleep" && len(dataPoints) > 0 {
		// Try to extract the start time from the first sleep session for a concrete hint.
		var dp map[string]interface{}
		if json.Unmarshal(dataPoints[0], &dp) == nil {
			if start, ok := dp["start"].(string); ok && start != "" {
				day := toUTC(start)[:10]
				hints = append(hints, req.Surface.pick(
					fmt.Sprintf("For overnight vitals: ghealth data heart-rate-variability list --from %s and ghealth data oxygen-saturation list --from %s", day, day),
					fmt.Sprintf("For overnight vitals, call query_data with operation 'list' and from '%s' on data_type 'heart-rate-variability', then on 'oxygen-saturation'.", day)))
			}
		}
	}

	// ── Hint 4: Empty results ────────────────────────────────────

	if nPoints == 0 {
		if token := nextPageToken(obj); token != "" {
			hints = append(hints, req.Surface.pick(
				"This page is empty but more data exists. The CLI auto-paginates — try increasing --limit.",
				fmt.Sprintf("This page is empty but more data exists — raise limit, or pass page_token '%s' to continue.", token)))
		} else if req.From != "" {
			hints = append(hints, req.Surface.pick(
				fmt.Sprintf("No %s data found for this range. Try a wider date range or check 'ghealth auth status' for scope coverage.", req.DataType),
				fmt.Sprintf("No %s data found for this range. Try a wider from/to range, or call auth_status to check that the granted scopes cover this type.", req.DataType)))
		}
	}

	return hints
}

// TruncationHint reports that a result stopped at the requested limit with more
// data behind it. It lives here rather than at the call site so both surfaces
// describe the continuation the same way they describe everything else.
func TruncationHint(surface Surface, limit int, token string) string {
	return surface.pick(
		fmt.Sprintf("returned %d rows = --limit; more data exists — fetch the next page with --page-token %s, "+
			"or raise --limit / narrow --from/--to", limit, token),
		fmt.Sprintf("returned %d rows = the requested limit; more data exists — fetch the next page with "+
			"page_token '%s', or raise limit / narrow from/to", limit, token))
}

// pick returns whichever phrasing belongs to this surface.
func (s Surface) pick(cli, mcp string) string {
	if s == SurfaceMCP {
		return mcp
	}
	return cli
}

// cliList renders a 'ghealth data <type> list' command line covering the same
// range as the call being hinted about. extra is appended verbatim (e.g. a
// --limit flag).
func cliList(dataType, from, to, extra string) string {
	cmd := "ghealth data " + dataType + " list"
	if from != "" {
		cmd += " --from " + from
	}
	if to != "" {
		cmd += " --to " + to
	}
	return cmd + extra
}

// toolList renders the query_data call equivalent to cliList. Argument values
// are single-quoted throughout, so one hint never mixes quoting styles.
func toolList(dataType, from, to, extra string) string {
	args := fmt.Sprintf("call query_data with data_type '%s', operation 'list'", dataType)
	if from != "" {
		args += fmt.Sprintf(", from '%s'", from)
	}
	if to != "" {
		args += fmt.Sprintf(", to '%s'", to)
	}
	return args + extra
}

// toUTC converts a local time string like "2026-03-29T14:18:32+01:00" to UTC "2026-03-29T13:18:32Z".
func toUTC(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return strings.TrimSuffix(s, "Z") + "Z"
	}
	return t.UTC().Format(time.RFC3339)
}

func nextPageToken(obj map[string]json.RawMessage) string {
	if tok, ok := obj["nextPageToken"]; ok {
		var t string
		json.Unmarshal(tok, &t)
		return t
	}
	return ""
}

// InjectHints adds a _hints field to a JSON response if there are hints.
func InjectHints(data json.RawMessage, hints []string) json.RawMessage {
	if len(hints) == 0 {
		return data
	}

	// Try to add _hints to an object.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err == nil {
		hintsJSON, _ := json.Marshal(hints)
		obj["_hints"] = hintsJSON
		out, _ := json.MarshalIndent(obj, "", "  ")
		return out
	}

	// For arrays (rollup), wrap in an object.
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err == nil {
		hintsJSON, _ := json.Marshal(hints)
		wrapper := map[string]json.RawMessage{
			"data":   data,
			"_hints": hintsJSON,
		}
		out, _ := json.MarshalIndent(wrapper, "", "  ")
		return out
	}

	return data
}
