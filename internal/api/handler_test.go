package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func postBodyRaw(t *testing.T, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/coordinate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func postBody(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	status, raw := postBodyRaw(t, body)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, raw)
	}
	return status, decoded
}

func TestCoordinateAccepted(t *testing.T) {
	body := `{"devices":[
		{"id":"mic-b","purpose":"ifb","center_khz":600000,"bandwidth_khz":201},
		{"id":"mic-a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	]}`
	status, resp := postBody(t, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200, resp = %v", status, resp)
	}
	if resp["accepted"] != true {
		t.Fatalf("accepted = %v; want true, resp = %v", resp["accepted"], resp)
	}
	devices, ok := resp["devices"].([]any)
	if !ok || len(devices) != 2 {
		t.Fatalf("devices = %v; want 2 entries", resp["devices"])
	}
	first := devices[0].(map[string]any)
	second := devices[1].(map[string]any)
	// Sorted by ID; odd bandwidth 201 -> occupied [599900, 600101], guard 250.
	if first["id"] != "mic-a" || first["low_khz"] != 499775.0 || first["high_khz"] != 500225.0 {
		t.Errorf("devices[0] = %v; want mic-a [499775, 500225]", first)
	}
	if second["id"] != "mic-b" || second["low_khz"] != 599650.0 || second["high_khz"] != 600351.0 {
		t.Errorf("devices[1] = %v; want mic-b [599650, 600351]", second)
	}
}

func TestCoordinateTouchingConflict(t *testing.T) {
	body := `{"devices":[
		{"id":"b","purpose":"handheld","center_khz":500450,"bandwidth_khz":200},
		{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	]}`
	status, resp := postBody(t, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200, resp = %v", status, resp)
	}
	if resp["accepted"] != false {
		t.Fatalf("accepted = %v; want false, resp = %v", resp["accepted"], resp)
	}
	conflicts, ok := resp["conflicts"].([]any)
	if !ok || len(conflicts) != 1 {
		t.Fatalf("conflicts = %v; want 1 pair", resp["conflicts"])
	}
	pair := conflicts[0].(map[string]any)
	if pair["first"] != "a" || pair["second"] != "b" {
		t.Errorf("pair = %v; want {first: a, second: b}", pair)
	}
	if oob, ok := resp["out_of_band"].([]any); !ok || len(oob) != 0 {
		t.Errorf("out_of_band = %v; want empty", resp["out_of_band"])
	}
}

func TestCoordinateOutOfBand(t *testing.T) {
	body := `{"devices":[
		{"id":"low","purpose":"ifb","center_khz":470200,"bandwidth_khz":100},
		{"id":"ok","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	]}`
	status, resp := postBody(t, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200, resp = %v", status, resp)
	}
	if resp["accepted"] != false {
		t.Fatalf("accepted = %v; want false, resp = %v", resp["accepted"], resp)
	}
	oob, ok := resp["out_of_band"].([]any)
	if !ok || len(oob) != 1 {
		t.Fatalf("out_of_band = %v; want 1 entry", resp["out_of_band"])
	}
	entry := oob[0].(map[string]any)
	// ifb guard 250: occupied [470150, 470250] -> protected [469900, 470500].
	if entry["id"] != "low" || entry["low_khz"] != 469900.0 || entry["high_khz"] != 470500.0 {
		t.Errorf("out_of_band[0] = %v; want low [469900, 470500]", entry)
	}
}

func TestCoordinateValidationErrorsLocateFields(t *testing.T) {
	body := `{"devices":[
		{"id":"dup","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
		{"id":"dup","purpose":"handheld","center_khz":500100,"bandwidth_khz":200},
		{"id":"x","purpose":"lavalier","center_khz":500000,"bandwidth_khz":200},
		{"id":"y","purpose":"handheld","center_khz":500000,"bandwidth_khz":24},
		{"id":"z","purpose":"handheld","center_khz":500000.5,"bandwidth_khz":200}
	]}`
	status, resp := postBody(t, body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400, resp = %v", status, resp)
	}
	if resp["accepted"] != false {
		t.Fatalf("accepted = %v; want false", resp["accepted"])
	}
	errs, ok := resp["errors"].([]any)
	if !ok {
		t.Fatalf("errors = %v; want a list", resp["errors"])
	}
	got := make(map[string]bool, len(errs))
	for _, e := range errs {
		got[e.(map[string]any)["field"].(string)] = true
	}
	for _, want := range []string{
		"devices[1].id",
		"devices[2].purpose",
		"devices[3].bandwidth_khz",
		"devices[4].center_khz",
	} {
		if !got[want] {
			t.Errorf("missing error for field %q; got fields %v", want, got)
		}
	}
}

func TestCoordinateFleetSizeLimits(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"devices":[]}`,
	} {
		status, resp := postBody(t, body)
		if status != http.StatusBadRequest {
			t.Errorf("body %s: status = %d; want 400, resp = %v", body, status, resp)
		}
	}
}

func TestCoordinateMalformedJSON(t *testing.T) {
	status, resp := postBody(t, `{"devices": [`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400, resp = %v", status, resp)
	}
	if resp["accepted"] != false {
		t.Errorf("accepted = %v; want false", resp["accepted"])
	}
}

// Regression: an int64-extreme center frequency must be rejected as out of
// band with a well-ordered interval, not wrapped around and accepted.
func TestCoordinateExtremeCenterFrequency(t *testing.T) {
	body := `{"devices":[
		{"id":"extreme","purpose":"handheld","center_khz":9223372036854775807,"bandwidth_khz":200},
		{"id":"normal","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	]}`
	status, raw := postBodyRaw(t, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200, body = %s", status, raw)
	}
	// Typed decode: the extreme endpoints exceed float64 precision.
	var resp struct {
		Accepted  bool `json:"accepted"`
		OutOfBand []struct {
			ID      string `json:"id"`
			LowKHz  int64  `json:"low_khz"`
			HighKHz int64  `json:"high_khz"`
		} `json:"out_of_band"`
		Conflicts []any `json:"conflicts"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Accepted {
		t.Fatalf("accepted = true; want false, body = %s", raw)
	}
	if len(resp.OutOfBand) != 1 || resp.OutOfBand[0].ID != "extreme" {
		t.Fatalf("out_of_band = %+v; want single entry for %q", resp.OutOfBand, "extreme")
	}
	iv := resp.OutOfBand[0]
	if iv.LowKHz > iv.HighKHz {
		t.Errorf("inverted interval [%d, %d]", iv.LowKHz, iv.HighKHz)
	}
	if iv.LowKHz != 9223372036854775582 || iv.HighKHz != 9223372036854775807 {
		t.Errorf("interval = [%d, %d]; want [9223372036854775582, 9223372036854775807]", iv.LowKHz, iv.HighKHz)
	}
	if len(resp.Conflicts) != 0 {
		t.Errorf("conflicts = %v; want empty", resp.Conflicts)
	}
}

func TestCoordinateCenterBeyondInt64(t *testing.T) {
	body := `{"devices":[{"id":"x","purpose":"handheld","center_khz":99999999999999999999999,"bandwidth_khz":200}]}`
	status, resp := postBody(t, body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400, resp = %v", status, resp)
	}
	errs, ok := resp["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("errors = %v; want a non-empty list", resp["errors"])
	}
	if errs[0].(map[string]any)["field"] != "devices[0].center_khz" {
		t.Errorf("error field = %v; want devices[0].center_khz", errs[0])
	}
}

// A lone device far from every edge is limited by the nearer band endpoint.
func TestCoordinateIncludeClearanceSingleDevice(t *testing.T) {
	status, raw := postBodyRaw(t, `{
		"include_clearance": true,
		"devices": [{"id":"solo","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]
	}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200, body = %s", status, raw)
	}
	var resp struct {
		Accepted  bool `json:"accepted"`
		Clearance struct {
			MinimumKHz int64 `json:"minimum_khz"`
			Devices    []struct {
				ID         string `json:"id"`
				MinimumKHz int64  `json:"minimum_khz"`
				Limiter    string `json:"limiter"`
			} `json:"devices"`
		} `json:"clearance"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Accepted {
		t.Fatalf("accepted = false; want true, body = %s", raw)
	}
	// Protected [499775, 500225]: band_low 29775, band_high 193775.
	if resp.Clearance.MinimumKHz != 29775 {
		t.Errorf("clearance.minimum_khz = %d; want 29775", resp.Clearance.MinimumKHz)
	}
	if len(resp.Clearance.Devices) != 1 {
		t.Fatalf("clearance.devices = %+v; want 1 entry", resp.Clearance.Devices)
	}
	d := resp.Clearance.Devices[0]
	if d.ID != "solo" || d.MinimumKHz != 29775 || d.Limiter != "band_low" {
		t.Errorf("clearance.devices[0] = %+v; want {solo 29775 band_low}", d)
	}
}

// Three devices submitted out of order: the global minimum comes from the
// gap around the middle device, and the response is byte-stable.
func TestCoordinateIncludeClearanceMiddleGap(t *testing.T) {
	body := `{
		"include_clearance": true,
		"devices": [
			{"id":"mic-c","purpose":"ifb","center_khz":600000,"bandwidth_khz":201},
			{"id":"mic-a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
			{"id":"mic-b","purpose":"handheld","center_khz":500500,"bandwidth_khz":200}
		]
	}`
	status, raw := postBodyRaw(t, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200, body = %s", status, raw)
	}
	var resp struct {
		Accepted  bool `json:"accepted"`
		Clearance struct {
			MinimumKHz int64 `json:"minimum_khz"`
			Devices    []struct {
				ID         string `json:"id"`
				MinimumKHz int64  `json:"minimum_khz"`
				Limiter    string `json:"limiter"`
			} `json:"devices"`
		} `json:"clearance"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Accepted {
		t.Fatalf("accepted = false; want true, body = %s", raw)
	}
	// Protected: a [499775,500225], b [500275,500725], c [599650,600351].
	// gap a<->b = 49 ticks; c band_high = 694000-600351 = 93649.
	type entry struct {
		id, limiter string
		min         int64
	}
	want := []entry{
		{"mic-a", "mic-b", 49},
		{"mic-b", "mic-a", 49},
		{"mic-c", "band_high", 93649},
	}
	if resp.Clearance.MinimumKHz != 49 {
		t.Errorf("clearance.minimum_khz = %d; want 49", resp.Clearance.MinimumKHz)
	}
	if len(resp.Clearance.Devices) != len(want) {
		t.Fatalf("clearance.devices = %+v; want %d entries", resp.Clearance.Devices, len(want))
	}
	for i, w := range want {
		got := resp.Clearance.Devices[i]
		if got.ID != w.id || got.MinimumKHz != w.min || got.Limiter != w.limiter {
			t.Errorf("clearance.devices[%d] = %+v; want {%s %d %s}", i, got, w.id, w.min, w.limiter)
		}
	}

	// Repeating the same request, and submitting the same fleet in a
	// different order, must yield byte-identical responses.
	_, repeat := postBodyRaw(t, body)
	if !bytes.Equal(raw, repeat) {
		t.Errorf("repeated request differs:\n%s\n%s", raw, repeat)
	}
	shuffled := `{
		"include_clearance": true,
		"devices": [
			{"id":"mic-b","purpose":"handheld","center_khz":500500,"bandwidth_khz":200},
			{"id":"mic-c","purpose":"ifb","center_khz":600000,"bandwidth_khz":201},
			{"id":"mic-a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
		]
	}`
	_, reordered := postBodyRaw(t, shuffled)
	if !bytes.Equal(raw, reordered) {
		t.Errorf("reordered fleet differs:\n%s\n%s", raw, reordered)
	}
}

// Omitted or false include_clearance keeps the legacy response
// byte-for-byte, for accepted and rejected fleets alike.
func TestCoordinateClearanceOmittedUnlessRequested(t *testing.T) {
	fleets := []string{
		`{"devices":[{"id":"solo","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]}`,
		`{"devices":[
			{"id":"b","purpose":"handheld","center_khz":500450,"bandwidth_khz":200},
			{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
		]}`,
	}
	for _, fleet := range fleets {
		status, legacy := postBodyRaw(t, fleet)
		if status != http.StatusOK {
			t.Fatalf("fleet %s: status = %d; want 200, body = %s", fleet, status, legacy)
		}
		if bytes.Contains(legacy, []byte(`"clearance"`)) {
			t.Errorf("fleet %s: response without the switch contains clearance: %s", fleet, legacy)
		}
		body := fleet[:len(fleet)-1] + `,"include_clearance":false}`
		vStatus, vRaw := postBodyRaw(t, body)
		if vStatus != status {
			t.Errorf("body %s: status = %d; want %d", body, vStatus, status)
		}
		if !bytes.Equal(legacy, vRaw) {
			t.Errorf("body %s: response = %s; want byte-identical to %s", body, vRaw, legacy)
		}
	}
}

// A rejected fleet reports only out-of-band and conflict details, even when
// clearance was requested.
func TestCoordinateClearanceOmittedOnRejection(t *testing.T) {
	status, raw := postBodyRaw(t, `{
		"include_clearance": true,
		"devices": [
			{"id":"b","purpose":"handheld","center_khz":500450,"bandwidth_khz":200},
			{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
		]
	}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200, body = %s", status, raw)
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["accepted"] != false {
		t.Fatalf("accepted = %v; want false, body = %s", resp["accepted"], raw)
	}
	if _, present := resp["clearance"]; present {
		t.Errorf("rejected response carries clearance: %s", raw)
	}
	if _, present := resp["conflicts"]; !present {
		t.Errorf("rejected response lost conflicts: %s", raw)
	}
}

// Duplicated object keys must be rejected as a 400 naming the field:
// encoding/json would otherwise keep the last value and adjudicate an
// ambiguous request on it.
func TestCoordinateDuplicateKeys(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{
			name: "clearance switch both off and on",
			body: `{
				"include_clearance": false,
				"include_clearance": true,
				"devices": [{"id":"solo","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]
			}`,
			field: "include_clearance",
		},
		{
			name: "two distinct device lists",
			body: `{
				"devices": [{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}],
				"devices": [{"id":"b","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]
			}`,
			field: "devices",
		},
		{
			name: "one device with two center frequencies",
			body: `{"devices":[
				{"id":"a","purpose":"handheld","center_khz":500000,"center_khz":500400,"bandwidth_khz":200}
			]}`,
			field: "devices[0].center_khz",
		},
		{
			name: "duplicate key on the second device",
			body: `{"devices":[
				{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
				{"id":"b","purpose":"handheld","center_khz":500000,"bandwidth_khz":200,"bandwidth_khz":100}
			]}`,
			field: "devices[1].bandwidth_khz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, resp := postBody(t, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400, resp = %v", status, resp)
			}
			if resp["accepted"] != false {
				t.Errorf("accepted = %v; want false", resp["accepted"])
			}
			errs, ok := resp["errors"].([]any)
			if !ok || len(errs) == 0 {
				t.Fatalf("errors = %v; want a non-empty list", resp["errors"])
			}
			got := make(map[string]bool, len(errs))
			for _, e := range errs {
				got[e.(map[string]any)["field"].(string)] = true
			}
			if !got[tc.field] {
				t.Errorf("missing error for field %q; got fields %v", tc.field, got)
			}
		})
	}
}

// Repeated identical values are still ambiguous: every duplicated key is a
// 400, and two problems in one body are both reported.
func TestCoordinateDuplicateKeysAllReported(t *testing.T) {
	body := `{
		"devices": [
			{"id":"a","purpose":"handheld","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
			{"id":"b","purpose":"handheld","center_khz":500000,"center_khz":500400,"bandwidth_khz":200}
		],
		"devices": [
			{"id":"c","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
		]
	}`
	status, resp := postBody(t, body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400, resp = %v", status, resp)
	}
	errs, ok := resp["errors"].([]any)
	if !ok {
		t.Fatalf("errors = %v; want a list", resp["errors"])
	}
	got := make(map[string]bool, len(errs))
	for _, e := range errs {
		got[e.(map[string]any)["field"].(string)] = true
	}
	for _, want := range []string{
		"devices",
		"devices[0].purpose",
		"devices[1].center_khz",
	} {
		if !got[want] {
			t.Errorf("missing error for field %q; got fields %v", want, got)
		}
	}
}

// A non-boolean include_clearance — null included — is a 400 that names the
// field, reported alongside any device-level problems.
func TestCoordinateIncludeClearanceTypeError(t *testing.T) {
	for _, value := range []string{`null`, `"yes"`, `1`, `{}`, `[]`} {
		body := `{"include_clearance":` + value + `,"devices":[{"id":"x","purpose":"lavalier","center_khz":500000,"bandwidth_khz":200}]}`
		status, resp := postBody(t, body)
		if status != http.StatusBadRequest {
			t.Fatalf("include_clearance=%s: status = %d; want 400, resp = %v", value, status, resp)
		}
		errs, ok := resp["errors"].([]any)
		if !ok {
			t.Fatalf("include_clearance=%s: errors = %v; want a list", value, resp["errors"])
		}
		got := make(map[string]bool, len(errs))
		for _, e := range errs {
			got[e.(map[string]any)["field"].(string)] = true
		}
		if !got["include_clearance"] {
			t.Errorf("include_clearance=%s: missing error for the switch itself; got fields %v", value, got)
		}
		if !got["devices[0].purpose"] {
			t.Errorf("include_clearance=%s: device-level error dropped; got fields %v", value, got)
		}
	}
}

// A string-typed number is a type error located at the exact field: a quoted
// integer such as "500000" must not silently enter the interval arithmetic,
// in either unit mode.
func TestCoordinateStringTypedNumbersRejected(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		field       string
		wantMessage string
	}{
		{
			name:        "string center in kHz mode",
			body:        `{"devices":[{"id":"a","purpose":"handheld","center_khz":"500000","bandwidth_khz":200}]}`,
			field:       "devices[0].center_khz",
			wantMessage: `must be an integer number of kHz, got "500000"`,
		},
		{
			name:        "string bandwidth in kHz mode",
			body:        `{"devices":[{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":"200"}]}`,
			field:       "devices[0].bandwidth_khz",
			wantMessage: `must be an integer number of kHz, got "200"`,
		},
		{
			name:        "string center in MHz mode",
			body:        `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":"500.000","bandwidth_khz":0.2}]}`,
			field:       "devices[0].center_khz",
			wantMessage: `must be a MHz number with at most three decimal places converting to an integer kHz, got "500.000"`,
		},
		{
			name:        "string bandwidth in MHz mode",
			body:        `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":500,"bandwidth_khz":"0.2"}]}`,
			field:       "devices[0].bandwidth_khz",
			wantMessage: `must be a MHz number with at most three decimal places converting to an integer kHz, got "0.2"`,
		},
		{
			name:  "non-numeric string center",
			body:  `{"devices":[{"id":"a","purpose":"handheld","center_khz":"abc","bandwidth_khz":200}]}`,
			field: "devices[0].center_khz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, resp := postBody(t, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400, resp = %v", status, resp)
			}
			if resp["accepted"] != false {
				t.Errorf("accepted = %v; want false", resp["accepted"])
			}
			errs, ok := resp["errors"].([]any)
			if !ok || len(errs) == 0 {
				t.Fatalf("errors = %v; want a non-empty list", resp["errors"])
			}
			var matched map[string]any
			for _, e := range errs {
				if fe := e.(map[string]any); fe["field"] == tc.field {
					matched = fe
				}
			}
			if matched == nil {
				t.Fatalf("missing error for %q; got %v", tc.field, resp["errors"])
			}
			if tc.wantMessage != "" && matched["message"] != tc.wantMessage {
				t.Errorf("message = %q; want %q", matched["message"], tc.wantMessage)
			}
		})
	}
}

// --- frequency_unit: mhz ---

// MHz numbers with up to three decimal places are converted exactly to
// integer kHz at the HTTP layer, then adjudicated by the unchanged rules.
// Odd bandwidths in MHz keep the asymmetric occupied interval:
// mic-b 600 MHz / 0.201 MHz -> 600000 / 201 -> [599650, 600351];
// mic-a 500 MHz / 0.2 MHz   -> 500000 / 200 -> [499775, 500225].
func TestCoordinateMHzAcceptedOddBandwidth(t *testing.T) {
	body := `{"frequency_unit":"mhz","devices":[
		{"id":"mic-b","purpose":"ifb","center_khz":600,"bandwidth_khz":0.201},
		{"id":"mic-a","purpose":"handheld","center_khz":500.000,"bandwidth_khz":0.2}
	]}`
	status, raw := postBodyRaw(t, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200, body = %s", status, raw)
	}
	wantBody := `{"accepted":true,"devices":[{"id":"mic-a","low_khz":499775,"high_khz":500225},{"id":"mic-b","low_khz":599650,"high_khz":600351}]}`
	if string(raw) != wantBody {
		t.Errorf("body = %s; want %s", raw, wantBody)
	}

	// The kHz-equivalent request must produce byte-identical output: the
	// conversion happens before adjudication and adds no response branch.
	kHzBody := `{"devices":[
		{"id":"mic-b","purpose":"ifb","center_khz":600000,"bandwidth_khz":201},
		{"id":"mic-a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	]}`
	_, kHzRaw := postBodyRaw(t, kHzBody)
	if !bytes.Equal(raw, kHzRaw) {
		t.Errorf("MHz and kHz responses differ:\n%s\n%s", raw, kHzRaw)
	}
}

// Two devices whose protected intervals touch at one endpoint conflict in
// MHz mode exactly as they do in kHz mode: handheld, 0.2 MHz bw -> ±0.225 MHz,
// centers 500.000 and 500.450 MHz meet at 500.225 MHz; 500.451 is accepted.
func TestCoordinateMHzTouchingEndpoints(t *testing.T) {
	touch := `{"frequency_unit":"mhz","devices":[
		{"id":"b","purpose":"handheld","center_khz":500.450,"bandwidth_khz":0.200},
		{"id":"a","purpose":"handheld","center_khz":500.000,"bandwidth_khz":0.200}
	]}`
	status, raw := postBodyRaw(t, touch)
	if status != http.StatusOK {
		t.Fatalf("touch: status = %d; want 200, body = %s", status, raw)
	}
	wantBody := `{"accepted":false,"conflicts":[{"first":"a","second":"b"}],"out_of_band":[]}`
	if string(raw) != wantBody {
		t.Errorf("touch: body = %s; want %s", raw, wantBody)
	}

	apart := `{"frequency_unit":"mhz","devices":[
		{"id":"b","purpose":"handheld","center_khz":500.451,"bandwidth_khz":0.200},
		{"id":"a","purpose":"handheld","center_khz":500.000,"bandwidth_khz":0.200}
	]}`
	status, raw = postBodyRaw(t, apart)
	if status != http.StatusOK {
		t.Fatalf("apart: status = %d; want 200, body = %s", status, raw)
	}
	var resp struct {
		Accepted  bool  `json:"accepted"`
		Conflicts []any `json:"conflicts"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Accepted || len(resp.Conflicts) != 0 {
		t.Errorf("apart: accepted=%v conflicts=%v, want accepted with no conflicts", resp.Accepted, resp.Conflicts)
	}
}

// Clearance is computed after the kHz conversion and its limiter sources are
// unchanged: the MHz response, including the clearance block, must be
// byte-identical to the kHz request for the same fleet.
func TestCoordinateMHzClearanceMatchesKHz(t *testing.T) {
	mhzBody := `{
		"frequency_unit": "mhz",
		"include_clearance": true,
		"devices": [
			{"id":"mic-c","purpose":"ifb","center_khz":600,"bandwidth_khz":0.201},
			{"id":"mic-a","purpose":"handheld","center_khz":500,"bandwidth_khz":0.2},
			{"id":"mic-b","purpose":"handheld","center_khz":500.5,"bandwidth_khz":0.2}
		]
	}`
	khzBody := `{
		"include_clearance": true,
		"devices": [
			{"id":"mic-c","purpose":"ifb","center_khz":600000,"bandwidth_khz":201},
			{"id":"mic-a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
			{"id":"mic-b","purpose":"handheld","center_khz":500500,"bandwidth_khz":200}
		]
	}`
	_, mhzRaw := postBodyRaw(t, mhzBody)
	_, khzRaw := postBodyRaw(t, khzBody)
	if !bytes.Equal(mhzRaw, khzRaw) {
		t.Errorf("MHz clearance response differs from kHz:\n%s\n%s", mhzRaw, khzRaw)
	}
	wantBody := `{"accepted":true,"clearance":{"minimum_khz":49,"devices":[{"id":"mic-a","minimum_khz":49,"limiter":"mic-b"},{"id":"mic-b","minimum_khz":49,"limiter":"mic-a"},{"id":"mic-c","minimum_khz":93649,"limiter":"band_high"}]},"devices":[{"id":"mic-a","low_khz":499775,"high_khz":500225},{"id":"mic-b","low_khz":500275,"high_khz":500725},{"id":"mic-c","low_khz":599650,"high_khz":600351}]}`
	if string(mhzRaw) != wantBody {
		t.Errorf("body = %s; want %s", mhzRaw, wantBody)
	}
}

// MHz precision, conversion and range failures are 400s located at the
// offending device property; the existing kHz bandwidth range still applies
// after exact conversion.
func TestCoordinateMHzValidationErrorsLocateFields(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		field       string
		wantMessage string // empty: only check field/status
	}{
		{
			name:        "center with four decimal places",
			body:        `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":500.0001,"bandwidth_khz":0.2}]}`,
			field:       "devices[0].center_khz",
			wantMessage: `must be a MHz number with at most three decimal places converting to an integer kHz, got 500.0001`,
		},
		{
			name:        "bandwidth with four decimal places",
			body:        `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":500,"bandwidth_khz":0.2005}]}`,
			field:       "devices[0].bandwidth_khz",
			wantMessage: `must be a MHz number with at most three decimal places converting to an integer kHz, got 0.2005`,
		},
		{
			name:  "exponent notation is not fixed point",
			body:  `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":5e2,"bandwidth_khz":0.2}]}`,
			field: "devices[0].center_khz",
		},
		{
			name:  "converted positive center beyond int64 kHz",
			body:  `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":9223372036854775.808,"bandwidth_khz":0.2}]}`,
			field: "devices[0].center_khz",
		},
		{
			name:  "converted negative center beyond int64 kHz",
			body:  `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":-9223372036854775.809,"bandwidth_khz":0.2}]}`,
			field: "devices[0].center_khz",
		},
		{
			name:        "converted bandwidth below 25 kHz",
			body:        `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":500,"bandwidth_khz":0.024}]}`,
			field:       "devices[0].bandwidth_khz",
			wantMessage: `must be between 25 and 400 kHz, got 24`,
		},
		{
			name:        "converted bandwidth above 400 kHz",
			body:        `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":500,"bandwidth_khz":0.401}]}`,
			field:       "devices[0].bandwidth_khz",
			wantMessage: `must be between 25 and 400 kHz, got 401`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, resp := postBody(t, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400, resp = %v", status, resp)
			}
			errs, ok := resp["errors"].([]any)
			if !ok || len(errs) == 0 {
				t.Fatalf("errors = %v; want a non-empty list", resp["errors"])
			}
			var matched any
			for _, e := range errs {
				fe := e.(map[string]any)
				if fe["field"] == tc.field {
					matched = fe
				}
			}
			if matched == nil {
				got := make([]string, 0, len(errs))
				for _, e := range errs {
					got = append(got, e.(map[string]any)["field"].(string))
				}
				t.Fatalf("missing error for %q; got fields %v", tc.field, got)
			}
			if tc.wantMessage != "" && matched.(map[string]any)["message"] != tc.wantMessage {
				t.Errorf("message = %q; want %q", matched.(map[string]any)["message"], tc.wantMessage)
			}
		})
	}
}

// Boundary values of the fixed-point conversion itself.
func TestParseMHzToKHz(t *testing.T) {
	cases := []struct {
		in     string
		want   int64
		reason string
	}{
		{"500", 500000, mhzOK},
		{"500.", 0, mhzBadPrecision}, // JSON never emits this; reject defensively
		{".5", 0, mhzBadPrecision},   // same
		{"500.0", 500000, mhzOK},
		{"0.025", 25, mhzOK},  // minimum legal bandwidth after conversion
		{"0.201", 201, mhzOK}, // odd kHz
		{"-0.001", -1, mhzOK},
		{"-500.450", -500450, mhzOK},
		{"500.0001", 0, mhzBadPrecision},
		{"-9223372036854.775808", 0, mhzBadPrecision}, // MinInt64 kHz would need six decimals
		{"500.00a", 0, mhzBadPrecision},
		{"5e2", 0, mhzBadPrecision},
		{"1e3", 0, mhzBadPrecision},
		{"9223372036854.775", 9223372036854775, mhzOK},       // inside int64 kHz
		{"9223372036854.776", 9223372036854776, mhzOK},       // inside int64 kHz
		{"9223372036854775.807", 9223372036854775807, mhzOK}, // exactly MaxInt64 kHz
		{"9223372036854775.808", 0, mhzOutOfIntRange},        // MaxInt64 + 1 kHz
		{"-9223372036854.776", -9223372036854776, mhzOK},
		{"-9223372036854775.808", -9223372036854775808, mhzOK}, // exactly MinInt64 kHz
		{"-9223372036854775.809", 0, mhzOutOfIntRange},         // MinInt64 - 1 kHz
		{"99999999999999", 99999999999999000, mhzOK},           // 17-digit kHz value, inside int64
		{"9999999999999999", 0, mhzOutOfIntRange},              // 19-digit kHz value, outside int64
		{"", 0, mhzMissing},
	}
	for _, tc := range cases {
		got, reason := parseMHzToKHz(tc.in)
		if reason != tc.reason || (reason == mhzOK && got != tc.want) {
			t.Errorf("parseMHzToKHz(%q) = (%d, %q); want (%d, %q)", tc.in, got, reason, tc.want, tc.reason)
		}
	}
}

// Unknown or mistyped frequency_unit values are refused before adjudication;
// the failure is located at the top-level field, and device-level problems in
// the same body are still reported.
func TestCoordinateFrequencyUnitErrors(t *testing.T) {
	for _, value := range []string{`"khz"`, `"GHz"`, `"MHz"`, `"hertz"`, `null`, `1`, `true`, `{}`} {
		body := `{"frequency_unit":` + value + `,"devices":[{"id":"x","purpose":"lavalier","center_khz":500000,"bandwidth_khz":200}]}`
		status, resp := postBody(t, body)
		if status != http.StatusBadRequest {
			t.Fatalf("frequency_unit=%s: status = %d; want 400, resp = %v", value, status, resp)
		}
		errs, ok := resp["errors"].([]any)
		if !ok {
			t.Fatalf("frequency_unit=%s: errors = %v; want a list", value, resp["errors"])
		}
		got := make(map[string]bool, len(errs))
		for _, e := range errs {
			got[e.(map[string]any)["field"].(string)] = true
		}
		if !got["frequency_unit"] {
			t.Errorf("frequency_unit=%s: missing error for the unit itself; got fields %v", value, got)
		}
		if !got["devices[0].purpose"] {
			t.Errorf("frequency_unit=%s: device-level error dropped; got fields %v", value, got)
		}
	}
}

// A duplicated frequency_unit is an ambiguous object key and is rejected by
// the duplicate-key scan before adjudication, even when both values agree.
func TestCoordinateDuplicateFrequencyUnit(t *testing.T) {
	body := `{
		"frequency_unit": "mhz",
		"frequency_unit": "mhz",
		"devices": [{"id":"a","purpose":"handheld","center_khz":500,"bandwidth_khz":0.2}]
	}`
	status, resp := postBody(t, body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400, resp = %v", status, resp)
	}
	errs, ok := resp["errors"].([]any)
	if !ok || len(errs) != 1 {
		t.Fatalf("errors = %v; want exactly one entry", resp["errors"])
	}
	if errs[0].(map[string]any)["field"] != "frequency_unit" {
		t.Errorf("field = %v; want frequency_unit", errs[0])
	}
}

// Without frequency_unit the legacy contract is byte-for-byte intact, both
// for successes (accepted/rejected) and for the 400 rejecting a fractional
// kHz number.
func TestCoordinateLegacyKHzBytesUnchanged(t *testing.T) {
	accepted := `{"devices":[{"id":"solo","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]}`
	status, raw := postBodyRaw(t, accepted)
	if status != http.StatusOK || string(raw) != `{"accepted":true,"devices":[{"id":"solo","low_khz":499775,"high_khz":500225}]}` {
		t.Errorf("accepted legacy body changed: %d %s", status, raw)
	}

	rejected := `{"devices":[{"id":"b","purpose":"handheld","center_khz":500450,"bandwidth_khz":200},{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]}`
	status, raw = postBodyRaw(t, rejected)
	if status != http.StatusOK || string(raw) != `{"accepted":false,"conflicts":[{"first":"a","second":"b"}],"out_of_band":[]}` {
		t.Errorf("rejected legacy body changed: %d %s", status, raw)
	}

	bad := `{"devices":[{"id":"w","purpose":"handheld","center_khz":500000.5,"bandwidth_khz":200}]}`
	status, raw = postBodyRaw(t, bad)
	if status != http.StatusBadRequest || string(raw) != `{"accepted":false,"errors":[{"field":"devices[0].center_khz","message":"must be an integer number of kHz, got 500000.5"}]}` {
		t.Errorf("legacy 400 body changed: %d %s", status, raw)
	}
}

// --- POST /v1/check-retunes ---

func postRetunesRaw(t *testing.T, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/check-retunes", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func postRetunes(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	status, raw := postRetunesRaw(t, body)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, raw)
	}
	return status, decoded
}

// other protects [500225, 500675]; the target currently touches it at 500225.
// Only candidate 499000 moves the target clear. Candidates are submitted out
// of order; the response is sorted by center frequency and byte-stable.
func TestCheckRetunesShuffledCandidatesStable(t *testing.T) {
	body := `{"devices":[
		{"id":"other","purpose":"handheld","center_khz":500450,"bandwidth_khz":200},
		{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	],"target_id":"target","candidate_centers_khz":[500450,499000,500000]}`
	status, raw := postRetunesRaw(t, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200, body = %s", status, raw)
	}
	want := `{"results":[` +
		`{"center_khz":499000,"accepted":true},` +
		`{"center_khz":500000,"accepted":false,"out_of_band":[],"conflicts":[{"first":"other","second":"target"}]},` +
		`{"center_khz":500450,"accepted":false,"out_of_band":[],"conflicts":[{"first":"other","second":"target"}]}` +
		`]}`
	if string(raw) != want {
		t.Errorf("body = %s; want %s", raw, want)
	}

	// Repeating the request, and submitting the same candidates in a
	// different order, must yield byte-identical responses.
	_, repeat := postRetunesRaw(t, body)
	if !bytes.Equal(raw, repeat) {
		t.Errorf("repeated request differs:\n%s\n%s", raw, repeat)
	}
	shuffled := `{"devices":[
		{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
		{"id":"other","purpose":"handheld","center_khz":500450,"bandwidth_khz":200}
	],"target_id":"target","candidate_centers_khz":[500000,500450,499000]}`
	_, reordered := postRetunesRaw(t, shuffled)
	if !bytes.Equal(raw, reordered) {
		t.Errorf("reordered candidates differ:\n%s\n%s", raw, reordered)
	}
}

// A conflict the target is not part of blocks every candidate: each trial is
// rejected and names the unrelated pair.
func TestCheckRetunesUnrelatedConflictBlocksAll(t *testing.T) {
	body := `{"devices":[
		{"id":"b","purpose":"handheld","center_khz":600100,"bandwidth_khz":200},
		{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
		{"id":"a","purpose":"handheld","center_khz":600000,"bandwidth_khz":200}
	],"target_id":"target","candidate_centers_khz":[501000,500000]}`
	status, raw := postRetunesRaw(t, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200, body = %s", status, raw)
	}
	want := `{"results":[` +
		`{"center_khz":500000,"accepted":false,"out_of_band":[],"conflicts":[{"first":"a","second":"b"}]},` +
		`{"center_khz":501000,"accepted":false,"out_of_band":[],"conflicts":[{"first":"a","second":"b"}]}` +
		`]}`
	if string(raw) != want {
		t.Errorf("body = %s; want %s", raw, want)
	}
}

// A legal candidate that pushes the target's protected interval out of the
// band is a trial verdict (200, accepted=false with the out-of-band detail),
// not a request error.
func TestCheckRetunesOutOfBandCandidateIsTrialVerdict(t *testing.T) {
	body := `{"devices":[
		{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	],"target_id":"target","candidate_centers_khz":[470000,500000]}`
	status, raw := postRetunesRaw(t, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200, body = %s", status, raw)
	}
	want := `{"results":[` +
		`{"center_khz":470000,"accepted":false,"out_of_band":[{"id":"target","low_khz":469775,"high_khz":470225}],"conflicts":[]},` +
		`{"center_khz":500000,"accepted":true}` +
		`]}`
	if string(raw) != want {
		t.Errorf("body = %s; want %s", raw, want)
	}
}

// MHz candidates and device numbers are converted to integer kHz before the
// trials run: the response is byte-identical to the equivalent kHz request.
func TestCheckRetunesMHzMatchesKHz(t *testing.T) {
	mhzBody := `{"frequency_unit":"mhz","devices":[
		{"id":"other","purpose":"handheld","center_khz":500.450,"bandwidth_khz":0.200},
		{"id":"target","purpose":"handheld","center_khz":500.000,"bandwidth_khz":0.200}
	],"target_id":"target","candidate_centers_khz":[500.450,499.000,500.000]}`
	khzBody := `{"devices":[
		{"id":"other","purpose":"handheld","center_khz":500450,"bandwidth_khz":200},
		{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	],"target_id":"target","candidate_centers_khz":[500450,499000,500000]}`
	status, mhzRaw := postRetunesRaw(t, mhzBody)
	if status != http.StatusOK {
		t.Fatalf("mhz: status = %d; want 200, body = %s", status, mhzRaw)
	}
	status, khzRaw := postRetunesRaw(t, khzBody)
	if status != http.StatusOK {
		t.Fatalf("khz: status = %d; want 200, body = %s", status, khzRaw)
	}
	if !bytes.Equal(mhzRaw, khzRaw) {
		t.Errorf("MHz and kHz trial responses differ:\n%s\n%s", mhzRaw, khzRaw)
	}
}

// Every malformed request element is a locatable 400; device-level problems
// in the same body are still reported alongside.
func TestCheckRetunesValidationErrorsLocateFields(t *testing.T) {
	fleet := `[{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]`
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"missing target_id", `{"devices":` + fleet + `,"candidate_centers_khz":[500000]}`, "target_id"},
		{"null target_id", `{"devices":` + fleet + `,"target_id":null,"candidate_centers_khz":[500000]}`, "target_id"},
		{"numeric target_id", `{"devices":` + fleet + `,"target_id":7,"candidate_centers_khz":[500000]}`, "target_id"},
		{"empty target_id", `{"devices":` + fleet + `,"target_id":"","candidate_centers_khz":[500000]}`, "target_id"},
		{"unknown target_id", `{"devices":` + fleet + `,"target_id":"ghost","candidate_centers_khz":[500000]}`, "target_id"},
		{"missing candidates", `{"devices":` + fleet + `,"target_id":"target"}`, "candidate_centers_khz"},
		{"empty candidates", `{"devices":` + fleet + `,"target_id":"target","candidate_centers_khz":[]}`, "candidate_centers_khz"},
		{"fractional kHz candidate", `{"devices":` + fleet + `,"target_id":"target","candidate_centers_khz":[500000.5]}`, "candidate_centers_khz[0]"},
		{"candidate beyond int64", `{"devices":` + fleet + `,"target_id":"target","candidate_centers_khz":[99999999999999999999999]}`, "candidate_centers_khz[0]"},
		{"duplicate candidates", `{"devices":` + fleet + `,"target_id":"target","candidate_centers_khz":[499000,500000,499000]}`, "candidate_centers_khz[2]"},
		{"mhz candidate with four decimals", `{"frequency_unit":"mhz","devices":` + fleet + `,"target_id":"target","candidate_centers_khz":[500.0001]}`, "candidate_centers_khz[0]"},
		{"mhz candidate beyond int64 kHz", `{"frequency_unit":"mhz","devices":` + fleet + `,"target_id":"target","candidate_centers_khz":[9223372036854775.808]}`, "candidate_centers_khz[0]"},
		{"mhz candidates normalize to the same kHz", `{"frequency_unit":"mhz","devices":` + fleet + `,"target_id":"target","candidate_centers_khz":[500,500.000]}`, "candidate_centers_khz[1]"},
		{"unknown frequency_unit", `{"frequency_unit":"khz","devices":` + fleet + `,"target_id":"target","candidate_centers_khz":[500000]}`, "frequency_unit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, resp := postRetunes(t, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400, resp = %v", status, resp)
			}
			if resp["accepted"] != false {
				t.Errorf("accepted = %v; want false", resp["accepted"])
			}
			errs, ok := resp["errors"].([]any)
			if !ok || len(errs) == 0 {
				t.Fatalf("errors = %v; want a non-empty list", resp["errors"])
			}
			got := make(map[string]bool, len(errs))
			for _, e := range errs {
				got[e.(map[string]any)["field"].(string)] = true
			}
			if !got[tc.field] {
				t.Errorf("missing error for field %q; got fields %v", tc.field, got)
			}
		})
	}

	// 51 candidates exceed the per-request limit.
	many := make([]string, 0, 51)
	for i := 0; i < 51; i++ {
		many = append(many, fmt.Sprintf("%d", 470137+i*1120))
	}
	body := `{"devices":` + fleet + `,"target_id":"target","candidate_centers_khz":[` + strings.Join(many, ",") + `]}`
	status, resp := postRetunes(t, body)
	if status != http.StatusBadRequest {
		t.Fatalf("51 candidates: status = %d; want 400, resp = %v", status, resp)
	}
	errs := resp["errors"].([]any)
	if errs[0].(map[string]any)["field"] != "candidate_centers_khz" {
		t.Errorf("51 candidates: error field = %v; want candidate_centers_khz", errs[0])
	}

	// A device-level problem is reported alongside a candidate problem.
	status, resp = postRetunes(t, `{"devices":[{"id":"target","purpose":"lavalier","center_khz":500000,"bandwidth_khz":200}],"target_id":"target","candidate_centers_khz":[500000.5]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("mixed errors: status = %d; want 400, resp = %v", status, resp)
	}
	got := map[string]bool{}
	for _, e := range resp["errors"].([]any) {
		got[e.(map[string]any)["field"].(string)] = true
	}
	for _, want := range []string{"devices[0].purpose", "candidate_centers_khz[0]"} {
		if !got[want] {
			t.Errorf("mixed errors: missing error for %q; got fields %v", want, got)
		}
	}
}

// A string-typed candidate is a locatable 400, not a trial: the quoted
// integer must be rejected against its own index, in either unit mode, and
// no trial runs on it.
func TestCheckRetunesStringCandidateRejected(t *testing.T) {
	fleet := `[{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]`
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"single string candidate", `{"devices":` + fleet + `,"target_id":"target","candidate_centers_khz":["499000"]}`, "candidate_centers_khz[0]"},
		{"string candidate among numbers", `{"devices":` + fleet + `,"target_id":"target","candidate_centers_khz":[499000,"500000"]}`, "candidate_centers_khz[1]"},
		{"mhz string candidate", `{"frequency_unit":"mhz","devices":` + fleet + `,"target_id":"target","candidate_centers_khz":["499.000"]}`, "candidate_centers_khz[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, resp := postRetunes(t, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400, resp = %v", status, resp)
			}
			if resp["accepted"] != false {
				t.Errorf("accepted = %v; want false", resp["accepted"])
			}
			errs, ok := resp["errors"].([]any)
			if !ok || len(errs) == 0 {
				t.Fatalf("errors = %v; want a non-empty list", resp["errors"])
			}
			got := make(map[string]bool, len(errs))
			for _, e := range errs {
				got[e.(map[string]any)["field"].(string)] = true
			}
			if !got[tc.field] {
				t.Errorf("missing error for field %q; got fields %v", tc.field, got)
			}
		})
	}

	// The exact message keeps the quotes so the type mistake stays visible.
	status, raw := postRetunesRaw(t, `{"devices":`+fleet+`,"target_id":"target","candidate_centers_khz":["499000"]}`)
	want := `{"accepted":false,"errors":[{"field":"candidate_centers_khz[0]","message":"must be an integer number of kHz, got \"499000\""}]}`
	if status != http.StatusBadRequest || string(raw) != want {
		t.Errorf("status = %d, body = %s; want 400 %s", status, raw, want)
	}
}

// Duplicated object keys in a retune request are rejected by the same scan
// as coordinate: two target selectors or two candidate lists are ambiguous.
func TestCheckRetunesDuplicateKeys(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{
			name: "two target selectors",
			body: `{"devices":[{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}],` +
				`"target_id":"target","target_id":"target","candidate_centers_khz":[500000]}`,
			field: "target_id",
		},
		{
			name: "two candidate lists",
			body: `{"devices":[{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}],` +
				`"target_id":"target","candidate_centers_khz":[500000],"candidate_centers_khz":[500100]}`,
			field: "candidate_centers_khz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, resp := postRetunes(t, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400, resp = %v", status, resp)
			}
			errs, ok := resp["errors"].([]any)
			if !ok || len(errs) == 0 {
				t.Fatalf("errors = %v; want a non-empty list", resp["errors"])
			}
			if errs[0].(map[string]any)["field"] != tc.field {
				t.Errorf("error field = %v; want %q", errs[0], tc.field)
			}
		})
	}
}

// The retune endpoint shares the coordinate request pipeline: malformed JSON
// and unknown fields are 400s, and include_clearance is not a retune field.
func TestCheckRetunesRequestShapeErrors(t *testing.T) {
	status, _ := postRetunesRaw(t, `{"devices": [`)
	if status != http.StatusBadRequest {
		t.Errorf("malformed JSON: status = %d; want 400", status)
	}
	status, resp := postRetunes(t, `{"devices":[{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}],"target_id":"target","candidate_centers_khz":[500000],"include_clearance":true}`)
	if status != http.StatusBadRequest {
		t.Errorf("unknown field include_clearance: status = %d; want 400, resp = %v", status, resp)
	}
}
