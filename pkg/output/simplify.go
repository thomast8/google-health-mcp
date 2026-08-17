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
	"strconv"
	"strings"
	"time"
)

// SimplifyResponse transforms a raw Health API response into a compact,
// agent-friendly format. Returns the original data unchanged if --raw is set.
func SimplifyResponse(data json.RawMessage, dataType string, raw bool) json.RawMessage {
	if raw {
		return data
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return data
	}

	// Handle list responses (dataPoints array).
	if dpRaw, ok := obj["dataPoints"]; ok {
		var dataPoints []map[string]interface{}
		if err := json.Unmarshal(dpRaw, &dataPoints); err != nil {
			return data
		}

		simplified := make([]map[string]interface{}, 0, len(dataPoints))
		for _, dp := range dataPoints {
			simplified = append(simplified, simplifyDataPoint(dp, dataType))
		}

		result := map[string]interface{}{"dataPoints": simplified}
		if tok, ok := obj["nextPageToken"]; ok {
			var t string
			json.Unmarshal(tok, &t)
			if t != "" {
				result["nextPageToken"] = t
			}
		}

		out, _ := json.MarshalIndent(result, "", "  ")
		return out
	}

	// Handle rollup responses (rollupDataPoints array).
	if rpRaw, ok := obj["rollupDataPoints"]; ok {
		var rollupPoints []map[string]interface{}
		if err := json.Unmarshal(rpRaw, &rollupPoints); err != nil {
			return data
		}

		simplified := make([]map[string]interface{}, 0, len(rollupPoints))
		for _, rp := range rollupPoints {
			s := simplifyRollupPoint(rp)
			// Skip rollup windows that carry only time fields and no metric
			// value. dailyRollUp points have "date" (or "startDate"/"endDate"
			// for multi-day windows); rollUp points have "start"/"end" —
			// count any other key as a real metric.
			metrics := 0
			for k := range s {
				switch k {
				case "date", "start", "end", "startDate", "endDate":
				default:
					metrics++
				}
			}
			if metrics == 0 {
				continue
			}
			simplified = append(simplified, s)
		}

		// rollUp paginates server-side (POST-with-body pageToken). A remaining
		// continuation token must survive simplification or the result looks
		// complete when it is not.
		if tok, ok := obj["nextPageToken"]; ok {
			var t string
			json.Unmarshal(tok, &t)
			if t != "" {
				result := map[string]interface{}{
					"rollupDataPoints": simplified,
					"nextPageToken":    t,
				}
				out, _ := json.MarshalIndent(result, "", "  ")
				return out
			}
		}

		out, _ := json.MarshalIndent(simplified, "", "  ")
		return out
	}

	// Not a recognized structure, return as-is.
	return data
}

// SimplifySleepResponse handles sleep-specific simplification with optional stage detail.
func SimplifySleepResponse(data json.RawMessage, includeStages bool, raw bool) json.RawMessage {
	if raw {
		return data
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return data
	}

	dpRaw, ok := obj["dataPoints"]
	if !ok {
		return data
	}

	var dataPoints []map[string]interface{}
	if err := json.Unmarshal(dpRaw, &dataPoints); err != nil {
		return data
	}

	simplified := make([]map[string]interface{}, 0, len(dataPoints))
	for _, dp := range dataPoints {
		simplified = append(simplified, simplifySleepPoint(dp, includeStages))
	}

	result := map[string]interface{}{"dataPoints": simplified}
	if tok, ok := obj["nextPageToken"]; ok {
		var t string
		json.Unmarshal(tok, &t)
		if t != "" {
			result["nextPageToken"] = t
		}
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	return out
}

func simplifyDataPoint(dp map[string]interface{}, dataType string) map[string]interface{} {
	result := make(map[string]interface{})

	// Find the type-specific data object (e.g., "heartRate", "weight", "steps").
	typeKey := findTypeKey(dp)
	if typeKey == "" {
		return dp
	}

	typeData, ok := dp[typeKey].(map[string]interface{})
	if !ok {
		return dp
	}

	// Extract timestamps based on structure.
	if iv, ok := typeData["interval"].(map[string]interface{}); ok {
		// Interval type: has startTime/endTime
		result["start"] = formatTimeWithOffset(iv, "startTime", "startUtcOffset")
		result["end"] = formatTimeWithOffset(iv, "endTime", "endUtcOffset")
	} else if st, ok := typeData["sampleTime"].(map[string]interface{}); ok {
		// Sample type: has physicalTime
		result["time"] = formatTimeWithOffset(st, "physicalTime", "utcOffset")
	} else if d, ok := typeData["date"].(map[string]interface{}); ok {
		// Daily type: has date object {year, month, day} → flatten to "YYYY-MM-DD"
		result["date"] = formatCivilDate(d)
	}

	// Extract all value fields (skip time-related ones).
	for k, v := range typeData {
		switch k {
		case "interval", "sampleTime", "date", "createTime", "updateTime":
			continue
		default:
			result[k] = numeric(k, v)
		}
	}

	// Compact source.
	result["source"] = extractSource(dp)

	// Include ID if present (needed for update/delete).
	if name, ok := dp["name"].(string); ok {
		parts := strings.Split(name, "/")
		result["id"] = parts[len(parts)-1]
	}

	return result
}

// formatCivilDate converts {year, month, day} to "YYYY-MM-DD".
func formatCivilDate(d map[string]interface{}) string {
	return fmt.Sprintf("%d-%02d-%02d", toInt(d["year"]), toInt(d["month"]), toInt(d["day"]))
}

// civilDateTime converts {year, month, day} to a time.Time (midnight UTC,
// for date arithmetic only).
func civilDateTime(d map[string]interface{}) time.Time {
	return time.Date(toInt(d["year"]), time.Month(toInt(d["month"])), toInt(d["day"]), 0, 0, 0, 0, time.UTC)
}

func simplifySleepPoint(dp map[string]interface{}, includeStages bool) map[string]interface{} {
	result := make(map[string]interface{})

	sleepData, ok := dp["sleep"].(map[string]interface{})
	if !ok {
		return dp
	}

	// Timestamps.
	if iv, ok := sleepData["interval"].(map[string]interface{}); ok {
		result["start"] = formatTimeWithOffset(iv, "startTime", "startUtcOffset")
		result["end"] = formatTimeWithOffset(iv, "endTime", "endUtcOffset")
	}

	// Sleep type.
	if t, ok := sleepData["type"]; ok {
		result["sleepType"] = t
	}

	// Metadata.
	if meta, ok := sleepData["metadata"].(map[string]interface{}); ok {
		if nap, ok := meta["nap"]; ok {
			result["isNap"] = nap
		}
	}

	// Summary (always include).
	if summary, ok := sleepData["summary"].(map[string]interface{}); ok {
		if v, ok := summary["minutesAsleep"]; ok {
			result["minutesAsleep"] = toInt(v)
		}
		if v, ok := summary["minutesAwake"]; ok {
			result["minutesAwake"] = toInt(v)
		}
		if v, ok := summary["minutesInSleepPeriod"]; ok {
			result["totalMinutes"] = toInt(v)
		}
		if v, ok := summary["minutesToFallAsleep"]; ok {
			result["minutesToFallAsleep"] = toInt(v)
		}

		// Stage summary as flat map.
		if stages, ok := summary["stagesSummary"].([]interface{}); ok {
			stageMap := make(map[string]int)
			for _, s := range stages {
				if sm, ok := s.(map[string]interface{}); ok {
					sType, _ := sm["type"].(string)
					mins := toInt(sm["minutes"])
					if sType != "" {
						stageMap[sType] = mins
					}
				}
			}
			result["stageMinutes"] = stageMap
		}
	}

	// Detailed stages (only with --detail).
	if includeStages {
		if stages, ok := sleepData["stages"].([]interface{}); ok {
			compactStages := make([]map[string]interface{}, 0, len(stages))
			for _, s := range stages {
				if sm, ok := s.(map[string]interface{}); ok {
					compact := map[string]interface{}{
						"type":  sm["type"],
						"start": formatTimeWithOffset(sm, "startTime", "startUtcOffset"),
						"end":   formatTimeWithOffset(sm, "endTime", "endUtcOffset"),
					}
					compactStages = append(compactStages, compact)
				}
			}
			result["stages"] = compactStages
		}
	}

	result["source"] = extractSource(dp)

	if name, ok := dp["name"].(string); ok {
		parts := strings.Split(name, "/")
		result["id"] = parts[len(parts)-1]
	}

	return result
}

func simplifyRollupPoint(rp map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	// dailyRollUp points carry a civil date; rollUp points carry physical
	// startTime/endTime instead.
	if cst, ok := rp["civilStartTime"].(map[string]interface{}); ok {
		if d, ok := cst["date"].(map[string]interface{}); ok {
			start := civilDateTime(d)
			result["date"] = formatCivilDate(d)
			// Multi-day windows (--window-days > 1) must not be mislabeled
			// as a single day. The civil interval is closed-open, so the
			// inclusive last day is civilEndTime minus one day; emit explicit
			// startDate/endDate instead of "date". 1-day buckets keep the
			// single "date" field for backward compatibility.
			if cet, ok := rp["civilEndTime"].(map[string]interface{}); ok {
				if ed, ok := cet["date"].(map[string]interface{}); ok {
					end := civilDateTime(ed)
					if end.Sub(start) > 24*time.Hour {
						delete(result, "date")
						result["startDate"] = formatCivilDate(d)
						last := end.AddDate(0, 0, -1)
						result["endDate"] = fmt.Sprintf("%d-%02d-%02d", last.Year(), last.Month(), last.Day())
					}
				}
			}
		}
	} else {
		if st, ok := rp["startTime"].(string); ok && st != "" {
			result["start"] = st
		}
		if et, ok := rp["endTime"].(string); ok && et != "" {
			result["end"] = et
		}
	}

	// Extract all value fields from the type-specific object.
	typeKey := findTypeKey(rp)
	if typeKey != "" {
		if typeData, ok := rp[typeKey].(map[string]interface{}); ok {
			for k, v := range typeData {
				result[k] = numeric(k, v)
			}
		}
	}

	return result
}

// numeric turns a measurement the API carries in a string into a real JSON
// number, recursing through nested objects and arrays.
//
// The Health API serializes every int64 field as a JSON string (protobuf's JSON
// mapping), so a step total arrives as "countSum": "1332" and a heart rate as
// "beatsPerMinute": "80". A consumer then has to know which quoted values are
// measurements and coerce them before it can compare, sum or plot them — and an
// agent reading one response has no way to tell "80" the number from "RUN" the
// enum. Emitting numbers as numbers moves that knowledge here, once.
//
// Two guards keep the conversion from ever losing or inventing information:
//
//   - The value must round-trip. A string becomes a number only if formatting
//     that number reproduces the original text exactly, so "007", "1.50" and
//     any int64 too large for a float64 stay strings rather than silently
//     changing.
//   - Identifier-shaped keys are left alone. A numeric id is not a measurement,
//     and turning it into a number would break the get call it is meant to feed.
func numeric(key string, v interface{}) interface{} {
	switch val := v.(type) {
	case string:
		if isIdentifierKey(key) {
			return v
		}
		if n, ok := asNumber(val); ok {
			return n
		}
		return v
	case map[string]interface{}:
		out := make(map[string]interface{}, len(val))
		for k, nested := range val {
			out[k] = numeric(k, nested)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(val))
		for i, elem := range val {
			// Array elements inherit the key their array hangs from, so
			// voltageSamples: ["1", "2"] converts and ids: ["1"] does not.
			out[i] = numeric(key, elem)
		}
		return out
	default:
		return v
	}
}

// asNumber parses a JSON number literal that reproduces itself when formatted
// back, returning the typed value and whether the conversion is safe.
func asNumber(s string) (interface{}, bool) {
	if s == "" {
		return nil, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && strconv.FormatInt(n, 10) == s {
		return n, true
	}
	// Reject the spellings JSON has no number for but Go's parser accepts
	// ("NaN", "Inf", "0x1p-2", "1_000", "+1") before trusting ParseFloat.
	if !isJSONNumber(s) {
		return nil, false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && strconv.FormatFloat(f, 'g', -1, 64) == s {
		return f, true
	}
	return nil, false
}

// isJSONNumber reports whether s is spelled exactly as JSON's number grammar
// allows: an optional minus, an integer part with no redundant leading zero, an
// optional fraction, and an optional exponent.
func isJSONNumber(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	// Integer part: "0", or a non-zero digit followed by digits.
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start || (i-start > 1 && s[start] == '0') {
		return false
	}
	// Fraction.
	if i < len(s) && s[i] == '.' {
		i++
		start = i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	// Exponent.
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start = i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(s)
}

// isIdentifierKey reports whether a field name holds a reference rather than a
// measurement. Health API identifiers are opaque strings that sometimes happen
// to be all digits, and a caller has to send them back verbatim.
func isIdentifierKey(key string) bool {
	switch key {
	case "id", "name", "uid", "guid":
		return true
	}
	for _, suffix := range []string{"Id", "ID", "Ids", "IDs", "Name", "Names", "Token", "Uid", "UID"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

// formatTimeWithOffset converts a UTC timestamp + offset into a local ISO 8601 string.
// The timestamp is a real UTC instant; the offset is added to it.
// Input: physicalTime "2026-03-29T16:05:00Z", utcOffset "3600s"
// Output: "2026-03-29T17:05:00+01:00"
func formatTimeWithOffset(obj map[string]interface{}, timeKey, offsetKey string) string {
	ts, _ := obj[timeKey].(string)
	if ts == "" {
		return ""
	}

	offsetStr, _ := obj[offsetKey].(string)
	offsetSec := parseOffsetSeconds(offsetStr)

	if offsetSec == 0 {
		return ts // Already UTC, keep Z suffix.
	}

	// The Health API serializes interval/sample times as real UTC instants
	// (the trailing "Z"). To render local wall-clock time the offset must be
	// ADDED to that instant — not merely appended to the same clock reading.
	// e.g. 08:29:48Z + 7200s → 10:29:48+02:00, not 08:29:48+02:00.
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts // Not parseable; leave untouched.
	}

	local := t.In(time.FixedZone("", offsetSec))

	// Preserve sub-second precision only when the input carried it.
	layout := "2006-01-02T15:04:05Z07:00"
	if strings.Contains(ts, ".") {
		layout = "2006-01-02T15:04:05.999999999Z07:00"
	}
	return local.Format(layout)
}

func parseOffsetSeconds(s string) int {
	s = strings.TrimSuffix(s, "s")
	n, _ := strconv.Atoi(s)
	return n
}

func findTypeKey(dp map[string]interface{}) string {
	for k := range dp {
		switch k {
		case "dataSource", "name", "civilStartTime", "civilEndTime":
			continue
		default:
			if _, ok := dp[k].(map[string]interface{}); ok {
				return k
			}
		}
	}
	return ""
}

func extractSource(dp map[string]interface{}) string {
	ds, ok := dp["dataSource"].(map[string]interface{})
	if !ok {
		return "unknown"
	}

	// Prefer device displayName.
	if dev, ok := ds["device"].(map[string]interface{}); ok {
		if name, ok := dev["displayName"].(string); ok && name != "" {
			return name
		}
	}

	// Fall back to app packageName.
	if app, ok := ds["application"].(map[string]interface{}); ok {
		if pkg, ok := app["packageName"].(string); ok && pkg != "" {
			return pkg
		}
	}

	// Fall back to platform.
	if p, ok := ds["platform"].(string); ok {
		return p
	}

	return "unknown"
}

func toInt(v interface{}) int {
	switch val := v.(type) {
	case float64:
		return int(val)
	case string:
		n, _ := strconv.Atoi(val)
		return n
	case int:
		return val
	}
	return 0
}
