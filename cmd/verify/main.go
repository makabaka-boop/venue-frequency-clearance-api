// Command verify is a one-shot acceptance probe: it waits for the API to
// become ready, exercises the coordination rules end to end, prints one
// PASS/FAIL line per check and exits non-zero if any check fails.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"
)

func main() {
	base := os.Getenv("API_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	if err := waitReady(base, 60*time.Second); err != nil {
		fmt.Println("FAIL readiness:", err)
		os.Exit(1)
	}

	checks := []struct {
		name string
		run  func(string) error
	}{
		{"accepted fleet, odd bandwidth, output sorted by id", checkAcceptedSorted},
		{"band edges are inclusive", checkBandEdges},
		{"touching endpoints conflict", checkTouchingConflict},
		{"one kHz apart is accepted", checkOneKHzApart},
		{"multiple conflicts sorted and normalized", checkMultipleConflicts},
		{"out-of-band devices reported with intervals", checkOutOfBand},
		{"extreme center saturates instead of wrapping", checkExtremeCenter},
		{"validation errors locate fields", checkValidationErrors},
		{"fleet size limits", checkFleetSizeLimits},
		{"verdict is stable across repeats", checkStable},
		{"clearance: single device limited by band edge", checkClearanceSingleDevice},
		{"clearance: middle gap of shuffled trio decides, stable", checkClearanceMiddleGap},
		{"clearance omitted on rejection even when requested", checkClearanceOmittedOnRejection},
		{"omitted/false include_clearance keeps legacy bytes", checkClearanceSwitchCompatible},
		{"include_clearance type error is a locatable 400", checkClearanceSwitchTypeError},
		{"duplicate keys (switch, device list, device field) are rejected", checkDuplicateKeys},
		{"mhz: odd-bandwidth fleet re-derives intervals and clearance", checkMHzOddBandwidth},
		{"mhz: touching endpoints conflict exactly like kHz", checkMHzTouchingConflict},
		{"mhz: illegal precision and range are locatable 400s", checkMHzValidationErrors},
		{"frequency_unit unknown or duplicated is rejected before adjudication", checkFrequencyUnitErrors},
		{"no frequency_unit: legacy success and rejection bytes unchanged", checkLegacyBytesUnchanged},
		{"retunes: shuffled candidates, one resolves the target conflict", checkRetunesResolves},
		{"retunes: unrelated conflict still blocks every candidate", checkRetunesUnrelatedConflict},
		{"retunes: out-of-band candidate is a trial verdict, not a 400", checkRetunesOutOfBandCandidate},
		{"retunes: MHz candidates match kHz trials byte-for-byte", checkRetunesMHzMatchesKHz},
		{"retunes: bad target, empty/duplicate/imprecise candidates are locatable 400s", checkRetunesValidationErrors},
		{"string-typed numbers are locatable 400s, never trial input", checkStringTypedNumbers},
		{"reconcile: shuffled inputs match one-to-one, missing and unexpected together", checkReconcileShuffledOneToOne},
		{"reconcile: closest pair wins deterministic ties, stable across repeats", checkReconcileTieBreak},
		{"reconcile: MHz observations match the equivalent kHz result byte-for-byte", checkReconcileMHzMatchesKHz},
		{"reconcile: illegal tolerance and records are locatable 400s, boundaries accepted", checkReconcileValidationErrors},
	}

	failures := 0
	for _, c := range checks {
		if err := c.run(base); err != nil {
			fmt.Printf("FAIL %s: %v\n", c.name, err)
			failures++
		} else {
			fmt.Printf("PASS %s\n", c.name)
		}
	}
	if failures > 0 {
		fmt.Printf("%d check(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("ALL CHECKS PASSED")
}

// --- wire types ---

type deviceReq struct {
	ID           string `json:"id"`
	Purpose      string `json:"purpose"`
	CenterKHz    any    `json:"center_khz"`
	BandwidthKHz any    `json:"bandwidth_khz"`
}

type deviceInterval struct {
	ID      string `json:"id"`
	LowKHz  int64  `json:"low_khz"`
	HighKHz int64  `json:"high_khz"`
}

type conflictPair struct {
	First  string `json:"first"`
	Second string `json:"second"`
}

type deviceClearance struct {
	ID         string `json:"id"`
	MinimumKHz int64  `json:"minimum_khz"`
	Limiter    string `json:"limiter"`
}

type clearanceBlock struct {
	MinimumKHz int64             `json:"minimum_khz"`
	Devices    []deviceClearance `json:"devices"`
}

type verdictResp struct {
	Accepted  bool             `json:"accepted"`
	Devices   []deviceInterval `json:"devices"`
	OutOfBand []deviceInterval `json:"out_of_band"`
	Conflicts []conflictPair   `json:"conflicts"`
	Clearance *clearanceBlock  `json:"clearance"`
}

type errorResp struct {
	Accepted bool `json:"accepted"`
	Errors   []struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	} `json:"errors"`
}

// --- checks ---

func checkAcceptedSorted(base string) error {
	// mic-b: ifb, bw 201 -> occupied [599900, 600101], guard 250 -> [599650, 600351].
	// mic-a: handheld, bw 200 -> occupied [499900, 500100], guard 125 -> [499775, 500225].
	status, raw, err := post(base, map[string]any{"devices": []deviceReq{
		{ID: "mic-b", Purpose: "ifb", CenterKHz: 600000, BandwidthKHz: 201},
		{ID: "mic-a", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
	}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if !v.Accepted {
		return fmt.Errorf("accepted = false, body = %s", raw)
	}
	want := []deviceInterval{
		{ID: "mic-a", LowKHz: 499775, HighKHz: 500225},
		{ID: "mic-b", LowKHz: 599650, HighKHz: 600351},
	}
	if !reflect.DeepEqual(v.Devices, want) {
		return fmt.Errorf("devices = %+v, want %+v", v.Devices, want)
	}
	return nil
}

func checkBandEdges(base string) error {
	// handheld, bw 25: half widths 12/13, guard 125.
	// edge-lo: 470137-12-125 = 470000 exactly; edge-hi: 693862+13+125 = 694000 exactly.
	status, raw, err := post(base, map[string]any{"devices": []deviceReq{
		{ID: "edge-hi", Purpose: "handheld", CenterKHz: 693862, BandwidthKHz: 25},
		{ID: "edge-lo", Purpose: "handheld", CenterKHz: 470137, BandwidthKHz: 25},
	}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if !v.Accepted {
		return fmt.Errorf("accepted = false, body = %s", raw)
	}
	want := []deviceInterval{
		{ID: "edge-hi", LowKHz: 693725, HighKHz: 694000},
		{ID: "edge-lo", LowKHz: 470000, HighKHz: 470275},
	}
	if !reflect.DeepEqual(v.Devices, want) {
		return fmt.Errorf("devices = %+v, want %+v", v.Devices, want)
	}
	return nil
}

func checkTouchingConflict(base string) error {
	// handheld, bw 200 -> center ± 225. a ends at 500225, b starts at 500225.
	status, raw, err := post(base, map[string]any{"devices": []deviceReq{
		{ID: "b", Purpose: "handheld", CenterKHz: 500450, BandwidthKHz: 200},
		{ID: "a", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
	}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.Accepted {
		return fmt.Errorf("accepted = true, want false, body = %s", raw)
	}
	want := []conflictPair{{First: "a", Second: "b"}}
	if !reflect.DeepEqual(v.Conflicts, want) {
		return fmt.Errorf("conflicts = %+v, want %+v", v.Conflicts, want)
	}
	if len(v.OutOfBand) != 0 {
		return fmt.Errorf("out_of_band = %+v, want empty", v.OutOfBand)
	}
	return nil
}

func checkOneKHzApart(base string) error {
	status, raw, err := post(base, map[string]any{"devices": []deviceReq{
		{ID: "a", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
		{ID: "b", Purpose: "handheld", CenterKHz: 500451, BandwidthKHz: 200},
	}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if !v.Accepted {
		return fmt.Errorf("accepted = false, want true, body = %s", raw)
	}
	return nil
}

func checkMultipleConflicts(base string) error {
	// Three mutually overlapping handheld mics, IDs submitted out of order.
	status, raw, err := post(base, map[string]any{"devices": []deviceReq{
		{ID: "gamma", Purpose: "handheld", CenterKHz: 500100, BandwidthKHz: 200},
		{ID: "alpha", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
		{ID: "beta", Purpose: "handheld", CenterKHz: 500050, BandwidthKHz: 200},
	}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.Accepted {
		return fmt.Errorf("accepted = true, want false, body = %s", raw)
	}
	want := []conflictPair{
		{First: "alpha", Second: "beta"},
		{First: "alpha", Second: "gamma"},
		{First: "beta", Second: "gamma"},
	}
	if !reflect.DeepEqual(v.Conflicts, want) {
		return fmt.Errorf("conflicts = %+v, want %+v", v.Conflicts, want)
	}
	return nil
}

func checkOutOfBand(base string) error {
	// z-lo: ifb, occupied [470150, 470250], guard 250 -> [469900, 470500]: low out.
	// z-hi: bodypack, occupied [693850, 693950], guard 175 -> [693675, 694125]: high out.
	status, raw, err := post(base, map[string]any{"devices": []deviceReq{
		{ID: "z-lo", Purpose: "ifb", CenterKHz: 470200, BandwidthKHz: 100},
		{ID: "ok", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
		{ID: "z-hi", Purpose: "bodypack", CenterKHz: 693900, BandwidthKHz: 100},
	}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.Accepted {
		return fmt.Errorf("accepted = true, want false, body = %s", raw)
	}
	want := []deviceInterval{
		{ID: "z-hi", LowKHz: 693675, HighKHz: 694125},
		{ID: "z-lo", LowKHz: 469900, HighKHz: 470500},
	}
	if !reflect.DeepEqual(v.OutOfBand, want) {
		return fmt.Errorf("out_of_band = %+v, want %+v", v.OutOfBand, want)
	}
	if len(v.Conflicts) != 0 {
		return fmt.Errorf("conflicts = %+v, want empty", v.Conflicts)
	}
	return nil
}

func checkExtremeCenter(base string) error {
	// int64-extreme centers must saturate, not wrap around into inverted,
	// seemingly in-band intervals. Both devices are out of band.
	status, raw, err := post(base, map[string]any{"devices": []deviceReq{
		{ID: "extreme-lo", Purpose: "handheld", CenterKHz: math.MinInt64, BandwidthKHz: 200},
		{ID: "extreme-hi", Purpose: "handheld", CenterKHz: math.MaxInt64, BandwidthKHz: 200},
		{ID: "normal", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
	}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.Accepted {
		return fmt.Errorf("accepted = true, want false, body = %s", raw)
	}
	want := []deviceInterval{
		{ID: "extreme-hi", LowKHz: math.MaxInt64 - 225, HighKHz: math.MaxInt64},
		{ID: "extreme-lo", LowKHz: math.MinInt64, HighKHz: math.MinInt64 + 225},
	}
	if !reflect.DeepEqual(v.OutOfBand, want) {
		return fmt.Errorf("out_of_band = %+v, want %+v", v.OutOfBand, want)
	}
	if len(v.Conflicts) != 0 {
		return fmt.Errorf("conflicts = %+v, want empty", v.Conflicts)
	}
	return nil
}

func checkValidationErrors(base string) error {
	status, raw, err := post(base, map[string]any{"devices": []deviceReq{
		{ID: "dup", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
		{ID: "dup", Purpose: "handheld", CenterKHz: 500100, BandwidthKHz: 200},
		{ID: "x", Purpose: "lavalier", CenterKHz: 500000, BandwidthKHz: 200},
		{ID: "y", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 24},
		{ID: "z", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 401},
		{ID: "w", Purpose: "handheld", CenterKHz: 500000.5, BandwidthKHz: 200},
		{ID: "", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
	}})
	if err != nil {
		return err
	}
	if status != http.StatusBadRequest {
		return fmt.Errorf("status = %d, want 400, body = %s", status, raw)
	}
	var e errorResp
	if err := json.Unmarshal(raw, &e); err != nil {
		return err
	}
	if e.Accepted {
		return fmt.Errorf("accepted = true, want false, body = %s", raw)
	}
	got := make(map[string]bool, len(e.Errors))
	for _, fe := range e.Errors {
		got[fe.Field] = true
	}
	for _, want := range []string{
		"devices[1].id",
		"devices[2].purpose",
		"devices[3].bandwidth_khz",
		"devices[4].bandwidth_khz",
		"devices[5].center_khz",
		"devices[6].id",
	} {
		if !got[want] {
			return fmt.Errorf("missing error for field %q, got fields %v, body = %s", want, got, raw)
		}
	}
	return nil
}

func checkFleetSizeLimits(base string) error {
	// Empty fleet must be rejected.
	status, raw, err := post(base, map[string]any{"devices": []deviceReq{}})
	if err != nil {
		return err
	}
	if status != http.StatusBadRequest {
		return fmt.Errorf("empty fleet: status = %d, want 400, body = %s", status, raw)
	}

	// 200 devices, 1120 kHz apart, all inside the band: must be accepted.
	fleet := make([]deviceReq, 0, 200)
	for i := 0; i < 200; i++ {
		fleet = append(fleet, deviceReq{
			ID:           fmt.Sprintf("dev-%03d", i),
			Purpose:      "handheld",
			CenterKHz:    470137 + int64(i)*1120,
			BandwidthKHz: 25,
		})
	}
	status, raw, err = post(base, map[string]any{"devices": fleet})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("200 devices: status = %d, want 200, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if !v.Accepted || len(v.Devices) != 200 {
		return fmt.Errorf("200 devices: accepted = %v, devices = %d, want true and 200", v.Accepted, len(v.Devices))
	}

	// 201 devices must be rejected as a validation error.
	fleet = append(fleet, deviceReq{ID: "dev-200", Purpose: "handheld", CenterKHz: 693900, BandwidthKHz: 25})
	status, raw, err = post(base, map[string]any{"devices": fleet})
	if err != nil {
		return err
	}
	if status != http.StatusBadRequest {
		return fmt.Errorf("201 devices: status = %d, want 400, body = %s", status, raw)
	}
	return nil
}

func checkStable(base string) error {
	body := map[string]any{"devices": []deviceReq{
		{ID: "gamma", Purpose: "handheld", CenterKHz: 500100, BandwidthKHz: 200},
		{ID: "alpha", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
		{ID: "beta", Purpose: "handheld", CenterKHz: 500050, BandwidthKHz: 200},
	}}
	_, first, err := post(base, body)
	if err != nil {
		return err
	}
	_, second, err := post(base, body)
	if err != nil {
		return err
	}
	if !bytes.Equal(first, second) {
		return fmt.Errorf("repeated identical requests differ:\n%s\n%s", first, second)
	}
	return nil
}

func checkClearanceSingleDevice(base string) error {
	// solo: handheld, 500000/200 -> protected [499775, 500225].
	// band_low margin 499775-470000 = 29775 beats band_high 694000-500225 = 193775.
	status, raw, err := post(base, map[string]any{
		"include_clearance": true,
		"devices": []deviceReq{
			{ID: "solo", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
		},
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if !v.Accepted {
		return fmt.Errorf("accepted = false, body = %s", raw)
	}
	if v.Clearance == nil {
		return fmt.Errorf("clearance missing from accepted response, body = %s", raw)
	}
	want := clearanceBlock{
		MinimumKHz: 29775,
		Devices:    []deviceClearance{{ID: "solo", MinimumKHz: 29775, Limiter: "band_low"}},
	}
	if !reflect.DeepEqual(*v.Clearance, want) {
		return fmt.Errorf("clearance = %+v, want %+v", *v.Clearance, want)
	}
	return nil
}

func checkClearanceMiddleGap(base string) error {
	// mic-a [499775,500225], mic-b [500275,500725], mic-c [599650,600351].
	// The a<->b gap is 500275-500225-1 = 49 unoccupied ticks, tighter than
	// every band margin; c is limited by band_high 694000-600351 = 93649.
	body := map[string]any{
		"include_clearance": true,
		"devices": []deviceReq{
			{ID: "mic-c", Purpose: "ifb", CenterKHz: 600000, BandwidthKHz: 201},
			{ID: "mic-a", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
			{ID: "mic-b", Purpose: "handheld", CenterKHz: 500500, BandwidthKHz: 200},
		},
	}
	status, raw, err := post(base, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if !v.Accepted {
		return fmt.Errorf("accepted = false, body = %s", raw)
	}
	if v.Clearance == nil {
		return fmt.Errorf("clearance missing from accepted response, body = %s", raw)
	}
	want := clearanceBlock{
		MinimumKHz: 49,
		Devices: []deviceClearance{
			{ID: "mic-a", MinimumKHz: 49, Limiter: "mic-b"},
			{ID: "mic-b", MinimumKHz: 49, Limiter: "mic-a"},
			{ID: "mic-c", MinimumKHz: 93649, Limiter: "band_high"},
		},
	}
	if !reflect.DeepEqual(*v.Clearance, want) {
		return fmt.Errorf("clearance = %+v, want %+v", *v.Clearance, want)
	}

	// The same request again, and the same fleet submitted in a different
	// order, must produce byte-identical responses.
	_, repeat, err := post(base, body)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, repeat) {
		return fmt.Errorf("repeated request differs:\n%s\n%s", raw, repeat)
	}
	reordered := map[string]any{
		"include_clearance": true,
		"devices": []deviceReq{
			{ID: "mic-b", Purpose: "handheld", CenterKHz: 500500, BandwidthKHz: 200},
			{ID: "mic-c", Purpose: "ifb", CenterKHz: 600000, BandwidthKHz: 201},
			{ID: "mic-a", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
		},
	}
	_, shuffled, err := post(base, reordered)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, shuffled) {
		return fmt.Errorf("reordered fleet differs:\n%s\n%s", raw, shuffled)
	}
	return nil
}

func checkClearanceOmittedOnRejection(base string) error {
	// Touching protected intervals: rejected; clearance must not appear
	// even though it was requested.
	status, raw, err := post(base, map[string]any{
		"include_clearance": true,
		"devices": []deviceReq{
			{ID: "b", Purpose: "handheld", CenterKHz: 500450, BandwidthKHz: 200},
			{ID: "a", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
		},
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.Accepted {
		return fmt.Errorf("accepted = true, want false, body = %s", raw)
	}
	if v.Clearance != nil || bytes.Contains(raw, []byte(`"clearance"`)) {
		return fmt.Errorf("rejected response carries clearance, body = %s", raw)
	}
	if len(v.Conflicts) != 1 {
		return fmt.Errorf("conflicts = %+v, want 1 pair", v.Conflicts)
	}
	return nil
}

func checkClearanceSwitchCompatible(base string) error {
	// Omitting the switch, or setting it to false, must leave the legacy
	// response byte-for-byte unchanged, accepted or rejected.
	fleets := []map[string]any{
		{"devices": []deviceReq{
			{ID: "solo", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
		}},
		{"devices": []deviceReq{
			{ID: "b", Purpose: "handheld", CenterKHz: 500450, BandwidthKHz: 200},
			{ID: "a", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
		}},
	}
	for i, fleet := range fleets {
		status, legacy, err := post(base, fleet)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("fleet %d: status = %d, body = %s", i, status, legacy)
		}
		if bytes.Contains(legacy, []byte(`"clearance"`)) {
			return fmt.Errorf("fleet %d: response without the switch contains clearance: %s", i, legacy)
		}
		withSwitch := map[string]any{"devices": fleet["devices"], "include_clearance": false}
		vStatus, vRaw, err := post(base, withSwitch)
		if err != nil {
			return err
		}
		if vStatus != status {
			return fmt.Errorf("fleet %d, switch false: status = %d, want %d", i, vStatus, status)
		}
		if !bytes.Equal(legacy, vRaw) {
			return fmt.Errorf("fleet %d, switch false: response = %s, want byte-identical to %s", i, vRaw, legacy)
		}
	}
	return nil
}

func checkClearanceSwitchTypeError(base string) error {
	// Non-boolean switch values — a string and null — are locatable 400s.
	for _, value := range []any{"yes", nil} {
		status, raw, err := post(base, map[string]any{
			"include_clearance": value,
			"devices": []deviceReq{
				{ID: "solo", Purpose: "handheld", CenterKHz: 500000, BandwidthKHz: 200},
			},
		})
		if err != nil {
			return err
		}
		if status != http.StatusBadRequest {
			return fmt.Errorf("switch %v: status = %d, want 400, body = %s", value, status, raw)
		}
		var e errorResp
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		found := false
		for _, fe := range e.Errors {
			if fe.Field == "include_clearance" {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("switch %v: no error locating include_clearance, body = %s", value, raw)
		}
	}
	return nil
}

// --- frequency_unit: mhz ---

func checkMHzOddBandwidth(base string) error {
	// mic-b ifb 600 MHz / 0.201 MHz -> occupied [599.900, 600.101], guard 0.250
	// -> protected [599.650, 600.351] MHz. mic-a handheld 500 MHz / 0.2 MHz ->
	// protected [499.775, 500.225] MHz. Include clearance so the recomputed
	// protection intervals and the limiter sources are both checked.
	status, raw, err := postRaw(base, `{
		"frequency_unit": "mhz",
		"include_clearance": true,
		"devices": [
			{"id": "mic-b", "purpose": "ifb",      "center_khz": 600,    "bandwidth_khz": 0.201},
			{"id": "mic-a", "purpose": "handheld", "center_khz": 500.000, "bandwidth_khz": 0.200}
		]
	}`)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if !v.Accepted {
		return fmt.Errorf("accepted = false, body = %s", raw)
	}
	want := []deviceInterval{
		{ID: "mic-a", LowKHz: 499775, HighKHz: 500225},
		{ID: "mic-b", LowKHz: 599650, HighKHz: 600351},
	}
	if !reflect.DeepEqual(v.Devices, want) {
		return fmt.Errorf("devices = %+v, want %+v", v.Devices, want)
	}
	if v.Clearance == nil {
		return fmt.Errorf("clearance missing, body = %s", raw)
	}
	// mic-a band_low 499775-470000 = 29775; mic-b band_high 694000-600351 =
	// 93649, tighter than the a<->b neighbor gap 599650-500225-1 = 99424.
	wantClearance := clearanceBlock{
		MinimumKHz: 29775,
		Devices: []deviceClearance{
			{ID: "mic-a", MinimumKHz: 29775, Limiter: "band_low"},
			{ID: "mic-b", MinimumKHz: 93649, Limiter: "band_high"},
		},
	}
	if !reflect.DeepEqual(*v.Clearance, wantClearance) {
		return fmt.Errorf("clearance = %+v, want %+v", *v.Clearance, wantClearance)
	}

	// The same fleet expressed in integer kHz, without frequency_unit, must
	// come back byte-for-byte identical: conversion precedes adjudication and
	// introduces no sorting/conflict/limiter branch.
	_, khzRaw, err := postRaw(base, `{
		"include_clearance": true,
		"devices": [
			{"id": "mic-b", "purpose": "ifb",      "center_khz": 600000, "bandwidth_khz": 201},
			{"id": "mic-a", "purpose": "handheld", "center_khz": 500000, "bandwidth_khz": 200}
		]
	}`)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, khzRaw) {
		return fmt.Errorf("MHz response differs from kHz response:\n%s\n%s", raw, khzRaw)
	}
	return nil
}

func checkMHzTouchingConflict(base string) error {
	// Two handhelds with 0.2 MHz bandwidth protect center ± 0.225 MHz. Centers
	// 500.000 and 500.450 MHz meet at 500.225 MHz: touching endpoints count as
	// a conflict, the same result the kHz request produces.
	touch := `{"frequency_unit":"mhz","devices":[
		{"id":"b","purpose":"handheld","center_khz":500.450,"bandwidth_khz":0.200},
		{"id":"a","purpose":"handheld","center_khz":500.000,"bandwidth_khz":0.200}
	]}`
	status, raw, err := postRaw(base, touch)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("touch: status = %d, body = %s", status, raw)
	}
	var v verdictResp
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.Accepted {
		return fmt.Errorf("touch: accepted = true, want conflict, body = %s", raw)
	}
	want := []conflictPair{{First: "a", Second: "b"}}
	if !reflect.DeepEqual(v.Conflicts, want) {
		return fmt.Errorf("touch: conflicts = %+v, want %+v", v.Conflicts, want)
	}
	if len(v.OutOfBand) != 0 {
		return fmt.Errorf("touch: out_of_band = %+v, want empty", v.OutOfBand)
	}

	// Identical to the legacy kHz request down to the bytes.
	_, khzRaw, err := postRaw(base, `{"devices":[
		{"id":"b","purpose":"handheld","center_khz":500450,"bandwidth_khz":200},
		{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	]}`)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, khzRaw) {
		return fmt.Errorf("touch: MHz response differs from kHz response:\n%s\n%s", raw, khzRaw)
	}

	// One kHz more separation (500.451 MHz) is accepted.
	status, apart, err := postRaw(base, `{"frequency_unit":"mhz","devices":[
		{"id":"b","purpose":"handheld","center_khz":500.451,"bandwidth_khz":0.200},
		{"id":"a","purpose":"handheld","center_khz":500.000,"bandwidth_khz":0.200}
	]}`)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("apart: status = %d, body = %s", status, apart)
	}
	var a verdictResp
	if err := json.Unmarshal(apart, &a); err != nil {
		return err
	}
	if !a.Accepted {
		return fmt.Errorf("apart: accepted = false, want true, body = %s", apart)
	}
	return nil
}

func checkMHzValidationErrors(base string) error {
	// Every illegal MHz input is a locatable 400 against the device property.
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{
			name:  "four decimal places on center",
			body:  `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":500.1234,"bandwidth_khz":0.2}]}`,
			field: "devices[0].center_khz",
		},
		{
			name:  "four decimal places on bandwidth",
			body:  `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":500,"bandwidth_khz":0.02499}]}`,
			field: "devices[0].bandwidth_khz",
		},
		{
			name:  "exponent notation is not decimal fixed point",
			body:  `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":5e2,"bandwidth_khz":0.2}]}`,
			field: "devices[0].center_khz",
		},
		{
			name:  "converted center beyond int64 kHz",
			body:  `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":9223372036854775.808,"bandwidth_khz":0.2}]}`,
			field: "devices[0].center_khz",
		},
		{
			name:  "converted bandwidth under 25 kHz",
			body:  `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":500,"bandwidth_khz":0.024}]}`,
			field: "devices[0].bandwidth_khz",
		},
		{
			name:  "converted bandwidth over 400 kHz",
			body:  `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":500,"bandwidth_khz":0.401}]}`,
			field: "devices[0].bandwidth_khz",
		},
		{
			name:  "two bad fields in one device are both reported",
			body:  `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":500.1234,"bandwidth_khz":0.401}]}`,
			field: "devices[0].bandwidth_khz",
		},
	}
	for _, tc := range cases {
		status, raw, err := postRaw(base, tc.body)
		if err != nil {
			return err
		}
		if status != http.StatusBadRequest {
			return fmt.Errorf("%s: status = %d, want 400, body = %s", tc.name, status, raw)
		}
		var e errorResp
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("%s: %w, body = %s", tc.name, err, raw)
		}
		found := false
		for _, fe := range e.Errors {
			if fe.Field == tc.field {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: no error locating %q, body = %s", tc.name, tc.field, raw)
		}
	}
	// The "two bad fields" case must locate both properties at once.
	status, raw, err := postRaw(base, cases[6].body)
	if err != nil {
		return err
	}
	if status != http.StatusBadRequest {
		return fmt.Errorf("two-fields case: status = %d, want 400, body = %s", status, raw)
	}
	var e errorResp
	if err := json.Unmarshal(raw, &e); err != nil {
		return err
	}
	got := map[string]bool{}
	for _, fe := range e.Errors {
		got[fe.Field] = true
	}
	for _, f := range []string{"devices[0].center_khz", "devices[0].bandwidth_khz"} {
		if !got[f] {
			return fmt.Errorf("two-fields case: missing error for %q, body = %s", f, raw)
		}
	}
	return nil
}

func checkFrequencyUnitErrors(base string) error {
	// Unknown units (including an explicit "khz" and the wrong case), null and
	// non-string types are refused before adjudication, located at the
	// top-level field...
	for _, value := range []string{`"ghz"`, `"MHz"`, `"khz"`, `null`, `1`, `true`} {
		status, raw, err := postRaw(base, `{"frequency_unit":`+value+`,"devices":[{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]}`)
		if err != nil {
			return err
		}
		if status != http.StatusBadRequest {
			return fmt.Errorf("frequency_unit=%s: status = %d, want 400, body = %s", value, status, raw)
		}
		var e errorResp
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		found := false
		for _, fe := range e.Errors {
			if fe.Field == "frequency_unit" {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("frequency_unit=%s: no error locating frequency_unit, body = %s", value, raw)
		}
	}
	// ...and a duplicated frequency_unit is an ambiguous key, rejected even
	// when both occurrences agree, without adjudicating either value.
	status, raw, err := postRaw(base, `{
		"frequency_unit": "mhz",
		"frequency_unit": "mhz",
		"devices": [{"id":"a","purpose":"handheld","center_khz":500,"bandwidth_khz":0.2}]
	}`)
	if err != nil {
		return err
	}
	if status != http.StatusBadRequest {
		return fmt.Errorf("duplicate unit: status = %d, want 400, body = %s", status, raw)
	}
	var e errorResp
	if err := json.Unmarshal(raw, &e); err != nil {
		return err
	}
	found := false
	for _, fe := range e.Errors {
		if fe.Field == "frequency_unit" {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("duplicate unit: no error locating frequency_unit, body = %s", raw)
	}
	return nil
}

func checkLegacyBytesUnchanged(base string) error {
	// With no frequency_unit, existing success and rejection responses remain
	// byte-for-byte what they always were: integer kHz in, kHz out.
	cases := []struct {
		name       string
		body       string
		wantStatus int
		want       string
	}{
		{
			name:       "accepted",
			body:       `{"devices":[{"id":"solo","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]}`,
			wantStatus: http.StatusOK,
			want:       `{"accepted":true,"devices":[{"id":"solo","low_khz":499775,"high_khz":500225}]}`,
		},
		{
			name:       "rejected conflict",
			body:       `{"devices":[{"id":"b","purpose":"handheld","center_khz":500450,"bandwidth_khz":200},{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]}`,
			wantStatus: http.StatusOK,
			want:       `{"accepted":false,"conflicts":[{"first":"a","second":"b"}],"out_of_band":[]}`,
		},
		{
			name:       "fractional kHz still refused",
			body:       `{"devices":[{"id":"w","purpose":"handheld","center_khz":500000.5,"bandwidth_khz":200}]}`,
			wantStatus: http.StatusBadRequest,
			want:       `{"accepted":false,"errors":[{"field":"devices[0].center_khz","message":"must be an integer number of kHz, got 500000.5"}]}`,
		},
	}
	for _, tc := range cases {
		status, raw, err := postRaw(base, tc.body)
		if err != nil {
			return err
		}
		if status != tc.wantStatus {
			return fmt.Errorf("%s: status = %d, want %d, body = %s", tc.name, status, tc.wantStatus, raw)
		}
		if !bytes.Equal(raw, []byte(tc.want)) {
			return fmt.Errorf("%s:\n got %s\nwant %s", tc.name, raw, tc.want)
		}
	}
	return nil
}

// --- retune trials ---

// retunesResp is the wire form of a check-retunes response.
type retunesResp struct {
	Results []struct {
		CenterKHz int64            `json:"center_khz"`
		Accepted  bool             `json:"accepted"`
		OutOfBand []deviceInterval `json:"out_of_band"`
		Conflicts []conflictPair   `json:"conflicts"`
	} `json:"results"`
}

func checkRetunesResolves(base string) error {
	// other protects [500225, 500675]; the target currently touches it at
	// 500225. Candidate 499000 moves the target to [498775, 499225]: the only
	// accepted trial. Candidates are submitted out of order; the response is
	// sorted by center frequency and byte-stable.
	body := `{"devices":[
		{"id":"other","purpose":"handheld","center_khz":500450,"bandwidth_khz":200},
		{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	],"target_id":"target","candidate_centers_khz":[500450,499000,500000]}`
	status, raw, err := postRetunes(base, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	want := `{"results":[` +
		`{"center_khz":499000,"accepted":true},` +
		`{"center_khz":500000,"accepted":false,"out_of_band":[],"conflicts":[{"first":"other","second":"target"}]},` +
		`{"center_khz":500450,"accepted":false,"out_of_band":[],"conflicts":[{"first":"other","second":"target"}]}` +
		`]}`
	if !bytes.Equal(raw, []byte(want)) {
		return fmt.Errorf("body = %s, want %s", raw, want)
	}

	// The same candidates in a different order, and the same fleet listed in
	// a different order, must produce byte-identical responses.
	shuffled := `{"devices":[
		{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
		{"id":"other","purpose":"handheld","center_khz":500450,"bandwidth_khz":200}
	],"target_id":"target","candidate_centers_khz":[500000,500450,499000]}`
	_, reordered, err := postRetunes(base, shuffled)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, reordered) {
		return fmt.Errorf("reordered candidates differ:\n%s\n%s", raw, reordered)
	}
	return nil
}

func checkRetunesUnrelatedConflict(base string) error {
	// a and b conflict with each other around 600 MHz; retuning the target
	// cannot fix that, so every candidate is rejected naming the a<->b pair.
	body := `{"devices":[
		{"id":"b","purpose":"handheld","center_khz":600100,"bandwidth_khz":200},
		{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
		{"id":"a","purpose":"handheld","center_khz":600000,"bandwidth_khz":200}
	],"target_id":"target","candidate_centers_khz":[501000,500000]}`
	status, raw, err := postRetunes(base, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, body = %s", status, raw)
	}
	want := `{"results":[` +
		`{"center_khz":500000,"accepted":false,"out_of_band":[],"conflicts":[{"first":"a","second":"b"}]},` +
		`{"center_khz":501000,"accepted":false,"out_of_band":[],"conflicts":[{"first":"a","second":"b"}]}` +
		`]}`
	if !bytes.Equal(raw, []byte(want)) {
		return fmt.Errorf("body = %s, want %s", raw, want)
	}
	return nil
}

func checkRetunesOutOfBandCandidate(base string) error {
	// Candidate 470000 is a legal kHz number, but the target's protected
	// interval [469775, 470225] leaves the band: a trial verdict (200 with
	// accepted=false and the out-of-band detail), not a request error.
	body := `{"devices":[
		{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	],"target_id":"target","candidate_centers_khz":[470000,500000]}`
	status, raw, err := postRetunes(base, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, want 200, body = %s", status, raw)
	}
	want := `{"results":[` +
		`{"center_khz":470000,"accepted":false,"out_of_band":[{"id":"target","low_khz":469775,"high_khz":470225}],"conflicts":[]},` +
		`{"center_khz":500000,"accepted":true}` +
		`]}`
	if !bytes.Equal(raw, []byte(want)) {
		return fmt.Errorf("body = %s, want %s", raw, want)
	}
	return nil
}

func checkRetunesMHzMatchesKHz(base string) error {
	// MHz candidates and device numbers are converted to integer kHz before
	// the trials run: the response is byte-identical to the kHz request.
	mhzBody := `{"frequency_unit":"mhz","devices":[
		{"id":"other","purpose":"handheld","center_khz":500.450,"bandwidth_khz":0.200},
		{"id":"target","purpose":"handheld","center_khz":500.000,"bandwidth_khz":0.200}
	],"target_id":"target","candidate_centers_khz":[500.450,499.000,500.000]}`
	status, mhzRaw, err := postRetunes(base, mhzBody)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("mhz: status = %d, body = %s", status, mhzRaw)
	}
	khzBody := `{"devices":[
		{"id":"other","purpose":"handheld","center_khz":500450,"bandwidth_khz":200},
		{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
	],"target_id":"target","candidate_centers_khz":[500450,499000,500000]}`
	status, khzRaw, err := postRetunes(base, khzBody)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("khz: status = %d, body = %s", status, khzRaw)
	}
	if !bytes.Equal(mhzRaw, khzRaw) {
		return fmt.Errorf("MHz and kHz trial responses differ:\n%s\n%s", mhzRaw, khzRaw)
	}
	return nil
}

func checkRetunesValidationErrors(base string) error {
	fleet := `[{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]`
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"missing target_id", `{"devices":` + fleet + `,"candidate_centers_khz":[500000]}`, "target_id"},
		{"null target_id", `{"devices":` + fleet + `,"target_id":null,"candidate_centers_khz":[500000]}`, "target_id"},
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
		status, raw, err := postRetunes(base, tc.body)
		if err != nil {
			return err
		}
		if status != http.StatusBadRequest {
			return fmt.Errorf("%s: status = %d, want 400, body = %s", tc.name, status, raw)
		}
		var e errorResp
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("%s: %w, body = %s", tc.name, err, raw)
		}
		if e.Accepted {
			return fmt.Errorf("%s: accepted = true, want false, body = %s", tc.name, raw)
		}
		found := false
		for _, fe := range e.Errors {
			if fe.Field == tc.field {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: no error locating %q, body = %s", tc.name, tc.field, raw)
		}
	}

	// 51 candidates exceed the per-request limit of 50.
	many := make([]string, 0, 51)
	for i := 0; i < 51; i++ {
		many = append(many, fmt.Sprintf("%d", 470137+i*1120))
	}
	status, raw, err := postRetunes(base, `{"devices":`+fleet+`,"target_id":"target","candidate_centers_khz":[`+strings.Join(many, ",")+`]}`)
	if err != nil {
		return err
	}
	if status != http.StatusBadRequest {
		return fmt.Errorf("51 candidates: status = %d, want 400, body = %s", status, raw)
	}
	var e errorResp
	if err := json.Unmarshal(raw, &e); err != nil {
		return err
	}
	found := false
	for _, fe := range e.Errors {
		if fe.Field == "candidate_centers_khz" {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("51 candidates: no error locating candidate_centers_khz, body = %s", raw)
	}
	return nil
}

// --- helpers ---

// checkStringTypedNumbers pins the numeric type contract: a quoted number
// such as "500000" is not a JSON number and must never reach the interval
// arithmetic or a retune trial — each is a 400 located at the exact field.
func checkStringTypedNumbers(base string) error {
	coordinateCases := []struct {
		name  string
		body  string
		field string
	}{
		{
			name:  "string device center",
			body:  `{"devices":[{"id":"a","purpose":"handheld","center_khz":"500000","bandwidth_khz":200}]}`,
			field: "devices[0].center_khz",
		},
		{
			name:  "string device bandwidth",
			body:  `{"devices":[{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":"200"}]}`,
			field: "devices[0].bandwidth_khz",
		},
		{
			name:  "string center in mhz mode",
			body:  `{"frequency_unit":"mhz","devices":[{"id":"a","purpose":"handheld","center_khz":"500.000","bandwidth_khz":0.2}]}`,
			field: "devices[0].center_khz",
		},
	}
	for _, tc := range coordinateCases {
		status, raw, err := postRaw(base, tc.body)
		if err != nil {
			return err
		}
		if err := expectFieldError(status, raw, tc.name, tc.field); err != nil {
			return err
		}
	}
	retuneCases := []struct {
		name  string
		body  string
		field string
	}{
		{
			name:  "string candidate",
			body:  `{"devices":[{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}],"target_id":"target","candidate_centers_khz":["499000"]}`,
			field: "candidate_centers_khz[0]",
		},
		{
			name:  "string candidate among numbers",
			body:  `{"devices":[{"id":"target","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}],"target_id":"target","candidate_centers_khz":[499000,"500000"]}`,
			field: "candidate_centers_khz[1]",
		},
	}
	for _, tc := range retuneCases {
		status, raw, err := postRetunes(base, tc.body)
		if err != nil {
			return err
		}
		if err := expectFieldError(status, raw, tc.name, tc.field); err != nil {
			return err
		}
	}
	return nil
}

// expectFieldError wants a 400 whose error list locates field.
func expectFieldError(status int, raw []byte, name, field string) error {
	if status != http.StatusBadRequest {
		return fmt.Errorf("%s: status = %d, want 400, body = %s", name, status, raw)
	}
	var e errorResp
	if err := json.Unmarshal(raw, &e); err != nil {
		return fmt.Errorf("%s: %w, body = %s", name, err, raw)
	}
	if e.Accepted {
		return fmt.Errorf("%s: accepted = true, want false, body = %s", name, raw)
	}
	for _, fe := range e.Errors {
		if fe.Field == field {
			return nil
		}
	}
	return fmt.Errorf("%s: no error locating %q, body = %s", name, field, raw)
}

func checkDuplicateKeys(base string) error {
	// Duplicated JSON object keys must not be silently last-wins: a request
	// carrying two clearance switches, two device lists, or two center
	// frequencies for one device is an ambiguous input and must be refused
	// with the offending field located.
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{
			name:  "both clearance switches",
			body:  `{"include_clearance":false,"include_clearance":true,"devices":[{"id":"solo","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]}`,
			field: "include_clearance",
		},
		{
			name:  "two device lists",
			body:  `{"devices":[{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}],"devices":[{"id":"b","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]}`,
			field: "devices",
		},
		{
			name:  "two center frequencies for one device",
			body:  `{"devices":[{"id":"a","purpose":"handheld","center_khz":500000,"center_khz":500400,"bandwidth_khz":200}]}`,
			field: "devices[0].center_khz",
		},
	}
	for _, tc := range cases {
		status, raw, err := postRaw(base, tc.body)
		if err != nil {
			return err
		}
		if status != http.StatusBadRequest {
			return fmt.Errorf("%s: status = %d, want 400, body = %s", tc.name, status, raw)
		}
		var e errorResp
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("%s: %w, body = %s", tc.name, err, raw)
		}
		if e.Accepted {
			return fmt.Errorf("%s: accepted = true, want false, body = %s", tc.name, raw)
		}
		found := false
		for _, fe := range e.Errors {
			if fe.Field == tc.field {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: no error locating %q, body = %s", tc.name, tc.field, raw)
		}
	}
	return nil
}

func postRaw(base, body string) (int, []byte, error) {
	resp, err := http.Post(base+"/v1/coordinate", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

func postRetunes(base, body string) (int, []byte, error) {
	resp, err := http.Post(base+"/v1/check-retunes", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

func post(base string, body any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.Post(base+"/v1/coordinate", "application/json", bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

// --- post-show observation reconciliation ---

// reconcileMatch is one matched entry in a reconcile response.
type reconcileMatch struct {
	DeviceID       string `json:"device_id"`
	ObservationID  string `json:"observation_id"`
	ObservedCenter int64  `json:"observed_center_khz"`
	DeviationKHz   int64  `json:"deviation_khz"`
}

type reconcileObservation struct {
	ID        string `json:"id"`
	CenterKHz int64  `json:"center_khz"`
}

type reconcileResp struct {
	Matched    []reconcileMatch       `json:"matched"`
	Missing    []string               `json:"missing"`
	Unexpected []reconcileObservation `json:"unexpected"`
}

func postReconcile(base, body string) (int, []byte, error) {
	resp, err := http.Post(base+"/v1/reconcile-observations", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

// checkReconcileShuffledOneToOne exercises the whole point of the endpoint:
// devices and observations arrive in arbitrary orders, the match is
// one-to-one, and a missing device and an extra carrier coexist in one
// result. Reordering both lists must not change a byte.
func checkReconcileShuffledOneToOne(base string) error {
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
	status, raw, err := postReconcile(base, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("status = %d, want 200, body = %s", status, raw)
	}
	var r reconcileResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	wantMatched := []reconcileMatch{
		{DeviceID: "a", ObservationID: "obs-1", ObservedCenter: 500010, DeviationKHz: 10},
		{DeviceID: "b", ObservationID: "obs-2", ObservedCenter: 500040, DeviationKHz: -10},
		{DeviceID: "c", ObservationID: "obs-3", ObservedCenter: 600000, DeviationKHz: 0},
	}
	if !reflect.DeepEqual(r.Matched, wantMatched) {
		return fmt.Errorf("matched = %+v, want %+v", r.Matched, wantMatched)
	}
	if !reflect.DeepEqual(r.Missing, []string{"d"}) {
		return fmt.Errorf("missing = %v, want [d]", r.Missing)
	}
	if !reflect.DeepEqual(r.Unexpected, []reconcileObservation{{ID: "obs-ghost", CenterKHz: 550000}}) {
		return fmt.Errorf("unexpected = %+v, want [obs-ghost]", r.Unexpected)
	}

	// Same fleet and carriers in different orders: byte-identical response.
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
	_, reordered, err := postReconcile(base, shuffled)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, reordered) {
		return fmt.Errorf("reordered input differs:\n%s\n%s", raw, reordered)
	}

	// One carrier within tolerance of two devices must go to the nearer one:
	// b at 500010 is 1 kHz from obs-near at 500009; a at 500000 is 9 kHz
	// away, so b occupies it and a goes missing (with tolerance 10).
	ambiguous := `{"tolerance_khz":10,"devices":[
		{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
		{"id":"b","purpose":"handheld","center_khz":500010,"bandwidth_khz":200}
	],"observations":[{"id":"obs-near","center_khz":500009}]}`
	status, raw, err = postReconcile(base, ambiguous)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("ambiguous: status = %d, want 200, body = %s", status, raw)
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	wantAmbiguous := []reconcileMatch{{DeviceID: "b", ObservationID: "obs-near", ObservedCenter: 500009, DeviationKHz: -1}}
	if !reflect.DeepEqual(r.Matched, wantAmbiguous) || !reflect.DeepEqual(r.Missing, []string{"a"}) || len(r.Unexpected) != 0 {
		return fmt.Errorf("ambiguous: matched=%+v missing=%v unexpected=%+v, want b/obs-near, [a], []", r.Matched, r.Missing, r.Unexpected)
	}
	return nil
}

// checkReconcileTieBreak pins the total ordering of candidate pairs: exact
// frequency ties break on device id then observation id, so two devices
// planned at one center and two equidistant carriers always pair a/o1, b/o2,
// whatever order they were submitted in.
func checkReconcileTieBreak(base string) error {
	cases := []string{
		`{"tolerance_khz":10,"devices":[
			{"id":"b","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
			{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
		],"observations":[
			{"id":"o2","center_khz":500005},
			{"id":"o1","center_khz":500005}
		]}`,
		`{"tolerance_khz":10,"devices":[
			{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200},
			{"id":"b","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}
		],"observations":[
			{"id":"o1","center_khz":500005},
			{"id":"o2","center_khz":500005}
		]}`,
	}
	want := `{"matched":[` +
		`{"device_id":"a","observation_id":"o1","observed_center_khz":500005,"deviation_khz":5},` +
		`{"device_id":"b","observation_id":"o2","observed_center_khz":500005,"deviation_khz":5}` +
		`],"missing":[],"unexpected":[]}`
	var first []byte
	for i, body := range cases {
		status, raw, err := postReconcile(base, body)
		if err != nil {
			return err
		}
		if status != http.StatusOK || string(raw) != want {
			return fmt.Errorf("case %d: got %d %s, want 200 %s", i, status, raw, want)
		}
		if i == 0 {
			first = raw
		} else if !bytes.Equal(first, raw) {
			return fmt.Errorf("tie orders differ:\n%s\n%s", first, raw)
		}
	}
	return nil
}

// checkReconcileMHzMatchesKHz verifies the unit conversion is reused end to
// end: device and observation centers in MHz are converted to integer kHz
// before matching, and the response is byte-identical to the kHz request.
// tolerance_khz stays an integer kHz even in MHz mode.
func checkReconcileMHzMatchesKHz(base string) error {
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
	status, mhzRaw, err := postReconcile(base, mhz)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("mhz: status = %d, want 200, body = %s", status, mhzRaw)
	}
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
	status, khzRaw, err := postReconcile(base, khz)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("khz: status = %d, want 200, body = %s", status, khzRaw)
	}
	if !bytes.Equal(mhzRaw, khzRaw) {
		return fmt.Errorf("MHz and kHz responses differ:\n%s\n%s", mhzRaw, khzRaw)
	}

	// A MHz observation with four decimals is a locatable 400, and a
	// fractional MHz tolerance is still an integer-kHz type error.
	status, raw, err := postReconcile(base, `{"frequency_unit":"mhz","tolerance_khz":100,"devices":[
		{"id":"a","purpose":"handheld","center_khz":500,"bandwidth_khz":0.2}
	],"observations":[{"id":"o1","center_khz":500.0001}]}`)
	if err != nil {
		return err
	}
	if err := expectReconcileFieldError(status, raw, "four-decimal observation", "observations[0].center_khz"); err != nil {
		return err
	}
	status, raw, err = postReconcile(base, `{"frequency_unit":"mhz","tolerance_khz":0.1,"devices":[
		{"id":"a","purpose":"handheld","center_khz":500,"bandwidth_khz":0.2}
	],"observations":[{"id":"o1","center_khz":500}]}`)
	if err != nil {
		return err
	}
	if err := expectReconcileFieldError(status, raw, "fractional tolerance", "tolerance_khz"); err != nil {
		return err
	}
	return nil
}

// checkReconcileValidationErrors pins every request-shape failure to its
// field with the same error envelope as the other endpoints, including the
// tolerance boundaries 0 and 10000 being accepted and observation list
// limits of 1 and 500.
func checkReconcileValidationErrors(base string) error {
	fleet := `[{"id":"a","purpose":"handheld","center_khz":500000,"bandwidth_khz":200}]`
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"missing tolerance", `{"devices":` + fleet + `,"observations":[{"id":"o1","center_khz":500000}]}`, "tolerance_khz"},
		{"fractional tolerance", `{"tolerance_khz":1.5,"devices":` + fleet + `,"observations":[{"id":"o1","center_khz":500000}]}`, "tolerance_khz"},
		{"negative tolerance", `{"tolerance_khz":-1,"devices":` + fleet + `,"observations":[{"id":"o1","center_khz":500000}]}`, "tolerance_khz"},
		{"tolerance above 10000", `{"tolerance_khz":10001,"devices":` + fleet + `,"observations":[{"id":"o1","center_khz":500000}]}`, "tolerance_khz"},
		{"string tolerance", `{"tolerance_khz":"100","devices":` + fleet + `,"observations":[{"id":"o1","center_khz":500000}]}`, "tolerance_khz"},
		{"null tolerance", `{"tolerance_khz":null,"devices":` + fleet + `,"observations":[{"id":"o1","center_khz":500000}]}`, "tolerance_khz"},
		{"missing observations", `{"tolerance_khz":100,"devices":` + fleet + `}`, "observations"},
		{"empty observations", `{"tolerance_khz":100,"devices":` + fleet + `,"observations":[]}`, "observations"},
		{"duplicate observation id", `{"tolerance_khz":100,"devices":` + fleet + `,"observations":[{"id":"x","center_khz":500000},{"id":"x","center_khz":500000}]}`, "observations[1].id"},
		{"empty observation id", `{"tolerance_khz":100,"devices":` + fleet + `,"observations":[{"id":"","center_khz":500000}]}`, "observations[0].id"},
		{"fractional observation center", `{"tolerance_khz":100,"devices":` + fleet + `,"observations":[{"id":"o1","center_khz":500000.5}]}`, "observations[0].center_khz"},
		{"string observation center", `{"tolerance_khz":100,"devices":` + fleet + `,"observations":[{"id":"o1","center_khz":"500000"}]}`, "observations[0].center_khz"},
		{"device validation reused", `{"tolerance_khz":100,"devices":[{"id":"a","purpose":"nope","center_khz":500000,"bandwidth_khz":24}],"observations":[{"id":"o1","center_khz":500000}]}`, "devices[0].purpose"},
		{"unknown frequency_unit", `{"frequency_unit":"khz","tolerance_khz":100,"devices":` + fleet + `,"observations":[{"id":"o1","center_khz":500000}]}`, "frequency_unit"},
		{"duplicated tolerance key", `{"tolerance_khz":100,"tolerance_khz":100,"devices":` + fleet + `,"observations":[{"id":"o1","center_khz":500000}]}`, "tolerance_khz"},
	}
	for _, tc := range cases {
		status, raw, err := postReconcile(base, tc.body)
		if err != nil {
			return err
		}
		if err := expectReconcileFieldError(status, raw, tc.name, tc.field); err != nil {
			return err
		}
	}

	// Tolerance boundaries 0 and 10000 are accepted; the frequency at the
	// exact boundary matches, one kHz past it does not.
	exact := `{"tolerance_khz":0,"devices":` + fleet + `,"observations":[{"id":"o1","center_khz":500000}]}`
	if status, raw, err := postReconcile(base, exact); err != nil {
		return err
	} else if status != http.StatusOK {
		return fmt.Errorf("tolerance 0: status = %d, want 200, body = %s", status, raw)
	}
	atMax := `{"tolerance_khz":10000,"devices":` + fleet + `,"observations":[{"id":"o1","center_khz":510000}]}`
	status, raw, err := postReconcile(base, atMax)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("tolerance 10000: status = %d, want 200, body = %s", status, raw)
	}
	var r reconcileResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if len(r.Matched) != 1 || r.Matched[0].DeviationKHz != 10000 {
		return fmt.Errorf("tolerance 10000 boundary: matched = %+v, want one match with deviation 10000", r.Matched)
	}

	// 500 observations is the largest accepted request; 501 locates the list.
	many := make([]string, 0, 501)
	for i := 0; i < 501; i++ {
		many = append(many, fmt.Sprintf(`{"id":"o-%03d","center_khz":500000}`, i))
	}
	status, raw, err = postReconcile(base, `{"tolerance_khz":100,"devices":`+fleet+`,"observations":[`+strings.Join(many[:500], ",")+`]}`)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("500 observations: status = %d, want 200, body = %s", status, raw)
	}
	status, raw, err = postReconcile(base, `{"tolerance_khz":100,"devices":`+fleet+`,"observations":[`+strings.Join(many, ",")+`]}`)
	if err != nil {
		return err
	}
	if err := expectReconcileFieldError(status, raw, "501 observations", "observations"); err != nil {
		return err
	}
	return nil
}

// expectReconcileFieldError wants a 400 error envelope whose list locates
// field and reports accepted=false.
func expectReconcileFieldError(status int, raw []byte, name, field string) error {
	if status != http.StatusBadRequest {
		return fmt.Errorf("%s: status = %d, want 400, body = %s", name, status, raw)
	}
	var e errorResp
	if err := json.Unmarshal(raw, &e); err != nil {
		return fmt.Errorf("%s: %w, body = %s", name, err, raw)
	}
	if e.Accepted {
		return fmt.Errorf("%s: accepted = true, want false, body = %s", name, raw)
	}
	for _, fe := range e.Errors {
		if fe.Field == field {
			return nil
		}
	}
	return fmt.Errorf("%s: no error locating %q, body = %s", name, field, raw)
}

func waitReady(base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("API at %s not ready within %s", base, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
