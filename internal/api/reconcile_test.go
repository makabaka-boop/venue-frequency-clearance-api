package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postReconcileRaw(t *testing.T, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/reconcile-observations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func postReconcile(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	status, raw := postReconcileRaw(t, body)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, raw)
	}
	return status, decoded
}

// Shuffled devices and observations still produce a deterministic one-to-one
// report; missing devices and extra carriers appear together.
func TestReconcileObservationsShuffledDeterministic(t *testing.T) {
	body := `{"tolerance_khz":100,"devices":[
		{"id":"d","purpose":"handheld","center_khz":650000,"bandwidth_khz":200},
		{"id":"b","purpose":"bodypack","center_khz":500050,"bandwidth_khz":100},
		{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
		{"id":"c","purpose":"ifb","center_khz":600000,"bandwidth_khz":200}
	],"observations":[
		{"id":"obs-2","center_khz":500040},
		{"id":"obs-ghost","center_khz":550000},
		{"id":"obs-3","center_khz":600000},
		{"id":"obs-1","center_khz":500010}
	]}`
	status, raw := postReconcileRaw(t, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200, body = %s", status, raw)
	}
	want := `{"matched":[` +
		`{"device_id":"a","observation_id":"obs-1","observed_center_khz":500010,"deviation_khz":10},` +
		`{"device_id":"b","observation_id":"obs-2","observed_center_khz":500040,"deviation_khz":-10},` +
		`{"device_id":"c","observation_id":"obs-3","observed_center_khz":600000,"deviation_khz":0}` +
		`],"missing":["d"],"unexpected":[{"id":"obs-ghost","center_khz":550000}]}`
	if string(raw) != want {
		t.Errorf("body = %s; want %s", raw, want)
	}

	// Submit devices and observations in different orders: byte-identical.
	shuffled := `{"tolerance_khz":100,"devices":[
		{"id":"c","purpose":"ifb","center_khz":600000,"bandwidth_khz":200},
		{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
		{"id":"d","purpose":"handheld","center_khz":650000,"bandwidth_khz":200},
		{"id":"b","purpose":"bodypack","center_khz":500050,"bandwidth_khz":100}
	],"observations":[
		{"id":"obs-3","center_khz":600000},
		{"id":"obs-1","center_khz":500010},
		{"id":"obs-ghost","center_khz":550000},
		{"id":"obs-2","center_khz":500040}
	]}`
	_, reordered := postReconcileRaw(t, shuffled)
	if !bytes.Equal(raw, reordered) {
		t.Errorf("reordered input differs:\n%s\n%s", raw, reordered)
	}
}

// Exact numeric ties (same planned center, equidistant carriers) break by
// (device id, observation id) regardless of submission order.
func TestReconcileObservationsTieBreaksByIds(t *testing.T) {
	body := `{"tolerance_khz":10,"devices":[
		{"id":"b","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
		{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	],"observations":[
		{"id":"o2","center_khz":500005},
		{"id":"o1","center_khz":500005}
	]}`
	want := `{"matched":[` +
		`{"device_id":"a","observation_id":"o1","observed_center_khz":500005,"deviation_khz":5},` +
		`{"device_id":"b","observation_id":"o2","observed_center_khz":500005,"deviation_khz":5}` +
		`],"missing":[],"unexpected":[]}`
	status, raw := postReconcileRaw(t, body)
	if status != http.StatusOK || string(raw) != want {
		t.Fatalf("got %d %s; want 200 %s", status, raw, want)
	}
}

// MHz device and observation numbers convert to integer kHz before matching;
// the response is byte-identical to the equivalent integer-kHz request.
func TestReconcileObservationsMHzMatchesKHz(t *testing.T) {
	mhz := `{"frequency_unit":"mhz","tolerance_khz":100,"devices":[
		{"id":"d","purpose":"handheld","center_khz":650,"bandwidth_khz":0.2},
		{"id":"b","purpose":"bodypack","center_khz":500.050,"bandwidth_khz":0.1},
		{"id":"a","purpose":"handheld","center_khz":500.000,"bandwidth_khz":0.2},
		{"id":"c","purpose":"ifb","center_khz":600,"bandwidth_khz":0.2}
	],"observations":[
		{"id":"obs-2","center_khz":500.040},
		{"id":"obs-ghost","center_khz":550},
		{"id":"obs-3","center_khz":600.000},
		{"id":"obs-1","center_khz":500.010}
	]}`
	khz := `{"tolerance_khz":100,"devices":[
		{"id":"d","purpose":"handheld","center_khz":650000,"bandwidth_khz":200},
		{"id":"b","purpose":"bodypack","center_khz":500050,"bandwidth_khz":100},
		{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
		{"id":"c","purpose":"ifb","center_khz":600000,"bandwidth_khz":200}
	],"observations":[
		{"id":"obs-2","center_khz":500040},
		{"id":"obs-ghost","center_khz":550000},
		{"id":"obs-3","center_khz":600000},
		{"id":"obs-1","center_khz":500010}
	]}`
	mStatus, mRaw := postReconcileRaw(t, mhz)
	kStatus, kRaw := postReconcileRaw(t, khz)
	if mStatus != http.StatusOK || kStatus != http.StatusOK {
		t.Fatalf("statuses mhz=%d khz=%d\nmhz: %s\nkhz: %s", mStatus, kStatus, mRaw, kRaw)
	}
	if !bytes.Equal(mRaw, kRaw) {
		t.Errorf("MHz and kHz responses differ:\n%s\n%s", mRaw, kRaw)
	}
}

// Nothing matches: every device is missing and every observation unexpected.
func TestReconcileObservationsAllUnmatched(t *testing.T) {
	body := `{"tolerance_khz":0,"devices":[
		{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	],"observations":[
		{"id":"o1","center_khz":500001}
	]}`
	status, raw := postReconcileRaw(t, body)
	want := `{"matched":[],"missing":["a"],"unexpected":[{"id":"o1","center_khz":500001}]}`
	if status != http.StatusOK || string(raw) != want {
		t.Fatalf("got %d %s; want 200 %s", status, raw, want)
	}
}

// tolerance_khz validation locates the field for every illegal shape.
func TestReconcileObservationsToleranceErrors(t *testing.T) {
	fleet := `[{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]`
	obs := `[{"id":"o1","center_khz":500000}]`
	cases := []struct {
		name        string
		tolerance   string
		wantMessage string
	}{
		{"missing", "", "is required and must be an integer number of kHz"},
		{"null", `null`, "is required and must be an integer number of kHz"},
		{"fractional", `1.5`, "must be an integer number of kHz, got 1.5"},
		{"negative", `-1`, "must be between 0 and 10000 kHz, got -1"},
		{"above maximum", `10001`, "must be between 0 and 10000 kHz, got 10001"},
		{"string", `"100"`, `must be an integer number of kHz, got "100"`},
		{"boolean", `true`, `must be an integer number of kHz, got true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"devices":` + fleet + `,"observations":` + obs
			if tc.tolerance != "" {
				body += `,"tolerance_khz":` + tc.tolerance
			}
			body += `}`
			status, resp := postReconcile(t, body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400, resp = %v", status, resp)
			}
			errs := resp["errors"].([]any)
			fe := errs[0].(map[string]any)
			if fe["field"] != "tolerance_khz" {
				t.Fatalf("field = %v; want tolerance_khz; errors = %v", fe["field"], errs)
			}
			if fe["message"] != tc.wantMessage {
				t.Errorf("message = %v; want %q", fe["message"], tc.wantMessage)
			}
		})
	}

	// Boundary values 0 and 10000 are accepted.
	for _, value := range []string{"0", "10000"} {
		body := `{"tolerance_khz":` + value + `,"devices":` + fleet + `,"observations":` + obs + `}`
		if status, raw := postReconcileRaw(t, body); status != http.StatusOK {
			t.Errorf("tolerance %s: status = %d; want 200, body = %s", value, status, raw)
		}
	}
}

// Observation records reuse the number rules; duplicate and empty ids are
// reported by index, and device validation is exactly the coordinate one.
func TestReconcileObservationsFieldErrors(t *testing.T) {
	body := `{"tolerance_khz":100,"devices":[
		{"id":"dup","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
		{"id":"dup","purpose":"nope","center_khz":500000.5,"bandwidth_khz":24}
	],"observations":[
		{"id":"same","center_khz":500000},
		{"id":"same","center_khz":500000},
		{"id":"","center_khz":"500000"},
		{"id":"bad-frac","center_khz":500000.25}
	]}`
	status, resp := postReconcile(t, body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400, resp = %v", status, resp)
	}
	got := map[string]bool{}
	for _, e := range resp["errors"].([]any) {
		got[e.(map[string]any)["field"].(string)] = true
	}
	for _, want := range []string{
		"devices[1].id",
		"devices[1].purpose",
		"devices[1].center_khz",
		"devices[1].bandwidth_khz",
		"observations[1].id",
		"observations[2].id",
		"observations[2].center_khz",
		"observations[3].center_khz",
	} {
		if !got[want] {
			t.Errorf("missing error for %q; got %v", want, got)
		}
	}
}

// Observation list size must be 1..500; both ends outside are locatable.
func TestReconcileObservationsSizeLimits(t *testing.T) {
	fleet := `[{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]`
	if status, resp := postReconcile(t, `{"tolerance_khz":100,"devices":`+fleet+`,"observations":[]}`); status != http.StatusBadRequest {
		t.Fatalf("empty: status = %d; want 400, resp = %v", status, resp)
	} else {
		errs := resp["errors"].([]any)
		if errs[0].(map[string]any)["field"] != "observations" {
			t.Errorf("empty: field = %v; want observations", errs[0])
		}
	}
	if status, _ := postReconcile(t, `{"tolerance_khz":100,"devices":`+fleet+`}`); status != http.StatusBadRequest {
		t.Fatalf("missing observations: status = %d; want 400", status)
	}

	many := make([]string, 0, 501)
	for i := 0; i < 501; i++ {
		many = append(many, fmt.Sprintf(`{"id":"o-%d","center_khz":500000}`, i))
	}
	status, resp := postReconcile(t, `{"tolerance_khz":100,"devices":`+fleet+`,"observations":[`+strings.Join(many, ",")+`]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("501 observations: status = %d; want 400", status)
	}
	found := false
	for _, e := range resp["errors"].([]any) {
		if e.(map[string]any)["field"] == "observations" {
			found = true
		}
	}
	if !found {
		t.Errorf("501 observations: no error locating observations, resp = %v", resp)
	}

	// 500 observations is accepted.
	ok := many[:500]
	status, raw := postReconcileRaw(t, `{"tolerance_khz":100,"devices":`+fleet+`,"observations":[`+strings.Join(ok, ",")+`]}`)
	if status != http.StatusOK {
		t.Fatalf("500 observations: status = %d; want 200, body = %s", status, raw)
	}
}

// Duplicated keys, an unknown frequency_unit and an unknown field are
// rejected by the same shared pipeline as the other endpoints.
func TestReconcileObservationsShapeErrors(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{
			name:  "duplicated tolerance",
			body:  `{"tolerance_khz":100,"tolerance_khz":100,"devices":[{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}],"observations":[{"id":"o1","center_khz":500000}]}`,
			field: "tolerance_khz",
		},
		{
			name:  "duplicated observations list",
			body:  `{"tolerance_khz":100,"devices":[{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}],"observations":[{"id":"o1","center_khz":500000}],"observations":[{"id":"o2","center_khz":500000}]}`,
			field: "observations",
		},
		{
			name:  "two observation ids",
			body:  `{"tolerance_khz":100,"devices":[{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}],"observations":[{"id":"o1","id":"o2","center_khz":500000}]}`,
			field: "observations[0].id",
		},
		{
			name:  "unknown frequency_unit",
			body:  `{"frequency_unit":"khz","tolerance_khz":100,"devices":[{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}],"observations":[{"id":"o1","center_khz":500000}]}`,
			field: "frequency_unit",
		},
		{
			name:  "unknown top-level field",
			body:  `{"include_clearance":true,"tolerance_khz":100,"devices":[{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}],"observations":[{"id":"o1","center_khz":500000}]}`,
			field: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := postReconcileRaw(t, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400, body = %s", status, raw)
			}
			if tc.field != "" {
				var resp struct {
					Errors []struct {
						Field string `json:"field"`
					} `json:"errors"`
				}
				if err := json.Unmarshal(raw, &resp); err != nil {
					t.Fatalf("decode: %v, body = %s", err, raw)
				}
				found := false
				for _, e := range resp.Errors {
					if e.Field == tc.field {
						found = true
					}
				}
				if !found {
					t.Errorf("no error locating %q; body = %s", tc.field, raw)
				}
			}
		})
	}
}
