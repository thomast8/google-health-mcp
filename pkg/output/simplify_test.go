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
	"testing"
)

// A rollup response carrying a nextPageToken must surface that token in the
// simplified output — dropping it silently misreports the result as complete.
func TestSimplifyResponse_RollupKeepsNextPageToken(t *testing.T) {
	raw := json.RawMessage(`{
		"rollupDataPoints": [
			{"startTime": "2026-03-01T00:00:00Z", "endTime": "2026-03-02T00:00:00Z", "steps": {"countSum": "9037"}}
		],
		"nextPageToken": "tok-abc"
	}`)

	out := SimplifyResponse(raw, "steps", false)

	var obj struct {
		RollupDataPoints []map[string]interface{} `json:"rollupDataPoints"`
		NextPageToken    string                   `json:"nextPageToken"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("simplified output with token should be an object: %v\n%s", err, out)
	}
	if obj.NextPageToken != "tok-abc" {
		t.Errorf("nextPageToken = %q, want tok-abc", obj.NextPageToken)
	}
	if len(obj.RollupDataPoints) != 1 {
		t.Errorf("got %d points, want 1", len(obj.RollupDataPoints))
	}
}

// Without a token, the rollup output stays a bare array (backward compat).
func TestSimplifyResponse_RollupNoTokenStaysArray(t *testing.T) {
	raw := json.RawMessage(`{
		"rollupDataPoints": [
			{"startTime": "2026-03-01T00:00:00Z", "endTime": "2026-03-02T00:00:00Z", "steps": {"countSum": "9037"}}
		]
	}`)

	out := SimplifyResponse(raw, "steps", false)

	var arr []map[string]interface{}
	if err := json.Unmarshal(out, &arr); err != nil {
		t.Fatalf("tokenless rollup output should remain an array: %v\n%s", err, out)
	}
	if len(arr) != 1 {
		t.Errorf("got %d points, want 1", len(arr))
	}
}

// A 1-day dailyRollUp bucket keeps the single "date" field (backward compat).
func TestSimplifyResponse_SingleDayBucketKeepsDate(t *testing.T) {
	raw := json.RawMessage(`{
		"rollupDataPoints": [
			{
				"civilStartTime": {"date": {"year": 2026, "month": 3, "day": 1}},
				"civilEndTime": {"date": {"year": 2026, "month": 3, "day": 2}},
				"steps": {"countSum": "9037"}
			}
		]
	}`)

	out := SimplifyResponse(raw, "steps", false)
	var arr []map[string]interface{}
	if err := json.Unmarshal(out, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("got %d points, want 1", len(arr))
	}
	if arr[0]["date"] != "2026-03-01" {
		t.Errorf("date = %v, want 2026-03-01", arr[0]["date"])
	}
	if _, ok := arr[0]["startDate"]; ok {
		t.Errorf("1-day bucket should not emit startDate")
	}
}

// A multi-day bucket (--window-days > 1) must not be mislabeled as a single
// day: emit startDate and endDate (inclusive) instead of "date".
func TestSimplifyResponse_MultiDayBucketEmitsStartEndDates(t *testing.T) {
	raw := json.RawMessage(`{
		"rollupDataPoints": [
			{
				"civilStartTime": {"date": {"year": 2026, "month": 3, "day": 1}},
				"civilEndTime": {"date": {"year": 2026, "month": 3, "day": 8}},
				"steps": {"countSum": "63000"}
			}
		]
	}`)

	out := SimplifyResponse(raw, "steps", false)
	var arr []map[string]interface{}
	if err := json.Unmarshal(out, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("got %d points, want 1", len(arr))
	}
	if _, ok := arr[0]["date"]; ok {
		t.Errorf("multi-day bucket must not emit a single date: %v", arr[0])
	}
	if arr[0]["startDate"] != "2026-03-01" {
		t.Errorf("startDate = %v, want 2026-03-01", arr[0]["startDate"])
	}
	// civilEndTime is the closed-open exclusive bound; the inclusive last
	// day of a Mar 1 - Mar 8 bucket is Mar 7.
	if arr[0]["endDate"] != "2026-03-07" {
		t.Errorf("endDate = %v, want 2026-03-07 (inclusive)", arr[0]["endDate"])
	}
}

// Valueless multi-day buckets are still dropped (presence semantics).
func TestSimplifyResponse_ValuelessMultiDayBucketDropped(t *testing.T) {
	raw := json.RawMessage(`{
		"rollupDataPoints": [
			{
				"civilStartTime": {"date": {"year": 2026, "month": 3, "day": 1}},
				"civilEndTime": {"date": {"year": 2026, "month": 3, "day": 8}}
			}
		]
	}`)

	out := SimplifyResponse(raw, "steps", false)
	var arr []map[string]interface{}
	if err := json.Unmarshal(out, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 0 {
		t.Errorf("valueless bucket should be dropped, got %v", arr)
	}
}

// The Health API sends every int64 as a quoted string. A caller then cannot tell
// "80" the measurement from "RUN" the enum without knowing the type's schema by
// heart, so measurements are emitted as numbers.
func TestSimplifyResponse_MeasurementsBecomeNumbers(t *testing.T) {
	data := json.RawMessage(`{
		"dataPoints": [
			{
				"name": "users/me/dataTypes/heartRate/dataPoints/dp1",
				"heartRate": {
					"sampleTime": {"physicalTime": "2026-08-17T15:33:07Z", "utcOffset": "3600s"},
					"beatsPerMinute": "80"
				},
				"dataSource": {"device": {"displayName": "Pixel Watch"}}
			}
		]
	}`)

	var got struct {
		DataPoints []map[string]json.RawMessage `json:"dataPoints"`
	}
	if err := json.Unmarshal(SimplifyResponse(data, "heart-rate", false), &got); err != nil {
		t.Fatalf("simplified output is not JSON: %v", err)
	}
	if len(got.DataPoints) != 1 {
		t.Fatalf("got %d points, want 1", len(got.DataPoints))
	}
	dp := got.DataPoints[0]

	if bpm := string(dp["beatsPerMinute"]); bpm != "80" {
		t.Errorf("beatsPerMinute is %s, want the unquoted number 80", bpm)
	}
	// Identity and provenance are not measurements and must stay strings.
	if id := string(dp["id"]); id != `"dp1"` {
		t.Errorf("id is %s, want a quoted string", id)
	}
	if src := string(dp["source"]); src != `"Pixel Watch"` {
		t.Errorf("source is %s, want a quoted string", src)
	}
}

func TestSimplifyResponse_RollupSumsBecomeNumbers(t *testing.T) {
	data := json.RawMessage(`{
		"rollupDataPoints": [
			{
				"civilStartTime": {"date": {"year": 2026, "month": 8, "day": 17}},
				"civilEndTime": {"date": {"year": 2026, "month": 8, "day": 18}},
				"steps": {"countSum": "1332"}
			}
		]
	}`)

	var got []map[string]json.RawMessage
	if err := json.Unmarshal(SimplifyResponse(data, "steps", false), &got); err != nil {
		t.Fatalf("simplified rollup is not JSON: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d buckets, want 1", len(got))
	}
	if sum := string(got[0]["countSum"]); sum != "1332" {
		t.Errorf("countSum is %s, want the unquoted number 1332", sum)
	}
	if date := string(got[0]["date"]); date != `"2026-08-17"` {
		t.Errorf("date is %s, want a quoted string", date)
	}
}

// --raw is documented as the original API response with nothing added and
// nothing changed, so the string-typed integers must survive it.
func TestSimplifyResponse_RawKeepsTheAPIsOwnTypes(t *testing.T) {
	data := json.RawMessage(`{"dataPoints":[{"steps":{"count":"1332"}}]}`)
	if got := string(SimplifyResponse(data, "steps", true)); got != string(data) {
		t.Errorf("--raw altered the response:\n got: %s\nwant: %s", got, data)
	}
}

// Nested measurements (an exercise's metrics summary, an ECG's samples) are
// converted too, since a caller reaching them has the same problem.
func TestNumericRecursesAndGuardsIdentifiers(t *testing.T) {
	got := numeric("metricsSummary", map[string]interface{}{
		"caloriesKcal":   "450",
		"distanceMeters": "8123.5",
		"segmentIds":     []interface{}{"1001", "1002"},
		"samples":        []interface{}{"1", "-2", "3"},
	})
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("numeric returned %T, want a map", got)
	}
	if m["caloriesKcal"] != int64(450) {
		t.Errorf("caloriesKcal is %#v, want int64(450)", m["caloriesKcal"])
	}
	if m["distanceMeters"] != 8123.5 {
		t.Errorf("distanceMeters is %#v, want 8123.5", m["distanceMeters"])
	}
	if ids := m["segmentIds"].([]interface{}); ids[0] != "1001" {
		t.Errorf("segmentIds[0] is %#v, want the string \"1001\" — identifiers are not measurements", ids[0])
	}
	if samples := m["samples"].([]interface{}); samples[1] != int64(-2) {
		t.Errorf("samples[1] is %#v, want int64(-2)", samples[1])
	}
}

// A conversion that would not reproduce the original text is not a conversion,
// it is a rewrite. Those values stay strings.
func TestAsNumberOnlyConvertsWhatRoundTrips(t *testing.T) {
	for _, s := range []string{"0", "1332", "-7", "8123.5", "0.5"} {
		if _, ok := asNumber(s); !ok {
			t.Errorf("asNumber(%q) refused a plain JSON number", s)
		}
	}
	for _, s := range []string{
		"",                   // empty
		"007",                // leading zeros would be dropped
		"1.50",               // trailing zero would be dropped
		"+1",                 // JSON has no leading plus
		"1_000",              // Go accepts the underscore, JSON does not
		"NaN", "Inf", "-Inf", // no JSON spelling
		"0x1p-2",           // Go hex float
		"9007199254740993", // an int64 no float64 can hold — but int64 takes it
		"3600s",            // a duration, not a number
		"2026-08-17",       // a date
		"RUN",              // an enum
		"1332 ",            // trailing space
	} {
		n, ok := asNumber(s)
		if s == "9007199254740993" {
			// Exact as an int64, so converting it loses nothing.
			if !ok || n != int64(9007199254740993) {
				t.Errorf("asNumber(%q) = %#v, %v; want the exact int64", s, n, ok)
			}
			continue
		}
		if ok {
			t.Errorf("asNumber(%q) converted to %#v, want it left as a string", s, n)
		}
	}
}
