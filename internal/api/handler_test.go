package api

import (
	"bytes"
	"encoding/json"
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
