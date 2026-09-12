package api

import (
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
