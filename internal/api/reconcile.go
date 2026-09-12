package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"wireless-coordinator/internal/rules"
)

// observationInput is the wire form of one analyzer carrier record. Numbers
// decode as numberInput for the same reasons as device frequencies:
// fractional/string-typed values must be reported against the exact field.
type observationInput struct {
	ID        string      `json:"id"`
	CenterKHz numberInput `json:"center_khz"`
}

// reconcileRequest is the wire form of a post-show reconciliation. The
// coordinator hands in the same device plan and the carriers the spectrum
// analyzer recorded; no release verdict is re-run.
type reconcileRequest struct {
	Devices      []deviceInput      `json:"devices"`
	Observations []observationInput `json:"observations"`
	// ToleranceKHz stays raw so a non-number, fractional or out-of-range
	// value can be reported against the field instead of failing the decode.
	ToleranceKHz numberInput `json:"tolerance_khz"`
	// FrequencyUnit behaves exactly as in coordinateRequest; when "mhz"
	// both device and observation center_khz numbers are converted to kHz.
	FrequencyUnit json.RawMessage `json:"frequency_unit"`
}

// reconcileMatchJSON is one occupied device/observation pair, ordered by
// device id in the response. Struct field order fixes the emitted key order.
type reconcileMatchJSON struct {
	DeviceID      string `json:"device_id"`
	ObservationID string `json:"observation_id"`
	ObservedKHz   int64  `json:"observed_center_khz"`
	DeviationKHz  int64  `json:"deviation_khz"`
}

// reconcileUnexpectedJSON is one observation no device was occupied for.
type reconcileUnexpectedJSON struct {
	ID        string `json:"id"`
	CenterKHz int64  `json:"center_khz"`
}

// handleReconcileObservations matches recorded carriers to the planned
// devices. It identifies devices left unpowered (missing), carriers recorded
// without a plan (unexpected) and the signed offset of every matched carrier;
// it never re-adjudicates clearance or conflicts.
func handleReconcileObservations(c *gin.Context) {
	var req reconcileRequest
	if !decodeRequestBody(c, &req) {
		return
	}

	reconDevices, observations, toleranceKHz, errs := validateReconcileRequest(req)
	if len(errs) > 0 {
		writeErrors(c, http.StatusBadRequest, errs...)
		return
	}

	result := rules.ReconcileObservations(reconDevices, observations, toleranceKHz)

	matched := make([]reconcileMatchJSON, 0, len(result.Matched))
	for _, m := range result.Matched {
		matched = append(matched, reconcileMatchJSON{
			DeviceID:      m.DeviceID,
			ObservationID: m.ObservationID,
			ObservedKHz:   m.ObservedCenter,
			DeviationKHz:  m.DeviationKHz,
		})
	}
	unexpected := make([]reconcileUnexpectedJSON, 0, len(result.Unexpected))
	for _, o := range result.Unexpected {
		unexpected = append(unexpected, reconcileUnexpectedJSON{ID: o.ID, CenterKHz: o.CenterKHz})
	}
	c.JSON(200, gin.H{
		"matched":    matched,
		"missing":    result.Missing,
		"unexpected": unexpected,
	})
}

// validateReconcileRequest checks the reconciliation request shape. Device
// parsing is the same validation as /v1/coordinate (id, purpose, bandwidth
// range and kHz/MHz number rules), so the plan is rejected for malformed
// devices even though the matching itself only needs id and center. Legal
// data that simply does not match — a missing device, an extra carrier — is
// never a request error: it goes straight into the reconciliation result.
func validateReconcileRequest(req reconcileRequest) ([]rules.ReconDevice, []rules.Observation, int64, []fieldError) {
	var errs []fieldError

	inMHz, unitOK := parseFrequencyUnit(req.FrequencyUnit)
	if !unitOK {
		errs = append(errs, fieldError{
			Field:   "frequency_unit",
			Message: fmt.Sprintf(`must be omitted or %q, got %s`, FrequencyUnitMHz, string(req.FrequencyUnit)),
		})
	}
	mhzMode := inMHz && unitOK

	devices, devErrs := validateDevices(req.Devices, mhzMode)
	errs = append(errs, devErrs...)

	observations, obsErrs := validateObservations(req.Observations, mhzMode)
	errs = append(errs, obsErrs...)

	toleranceKHz, tolErrs := validateTolerance(req.ToleranceKHz)
	errs = append(errs, tolErrs...)

	// Build the domain's slim device view from the fully validated plan.
	reconDevices := make([]rules.ReconDevice, 0, len(devices))
	for _, d := range devices {
		reconDevices = append(reconDevices, rules.ReconDevice{ID: d.ID, CenterKHz: d.CenterKHz})
	}
	return reconDevices, observations, toleranceKHz, errs
}

// validateTolerance reads the required tolerance_khz selector. It is always
// an integer number of kHz, independently of frequency_unit: it compares
// already-converted kHz centers. Fractional numbers, quoted strings and
// values outside [0, 10000] are reported against the field.
func validateTolerance(n numberInput) (int64, []fieldError) {
	if n.String() == "" {
		return 0, []fieldError{{Field: "tolerance_khz", Message: "is required and must be an integer number of kHz"}}
	}
	tolerance, ok := parseFrequency(n, false)
	if !ok {
		return 0, []fieldError{{Field: "tolerance_khz", Message: frequencyProblem(n, false)}}
	}
	if tolerance < rules.MinToleranceKHz || tolerance > rules.MaxToleranceKHz {
		return 0, []fieldError{{
			Field:   "tolerance_khz",
			Message: fmt.Sprintf("must be between %d and %d kHz, got %d", rules.MinToleranceKHz, rules.MaxToleranceKHz, tolerance),
		}}
	}
	return tolerance, nil
}

// validateObservations checks the observation list size and every record,
// collecting one error per problem in the same style as validateDevices.
func validateObservations(inputs []observationInput, mhzMode bool) ([]rules.Observation, []fieldError) {
	var errs []fieldError

	switch {
	case inputs == nil:
		return nil, []fieldError{{Field: "observations", Message: "is required"}}
	case len(inputs) < rules.MinObservations || len(inputs) > rules.MaxObservations:
		errs = append(errs, fieldError{
			Field:   "observations",
			Message: fmt.Sprintf("must contain between %d and %d observations, got %d", rules.MinObservations, rules.MaxObservations, len(inputs)),
		})
	}

	observations := make([]rules.Observation, 0, len(inputs))
	seen := make(map[string]int, len(inputs))
	for i, in := range inputs {
		prefix := fmt.Sprintf("observations[%d]", i)

		if in.ID == "" {
			errs = append(errs, fieldError{Field: prefix + ".id", Message: "must not be empty"})
		} else if prev, dup := seen[in.ID]; dup {
			errs = append(errs, fieldError{
				Field:   prefix + ".id",
				Message: fmt.Sprintf("duplicates observations[%d].id %q", prev, in.ID),
			})
		} else {
			seen[in.ID] = i
		}

		center, centerOK := parseFrequency(in.CenterKHz, mhzMode)
		if !centerOK {
			errs = append(errs, fieldError{Field: prefix + ".center_khz", Message: frequencyProblem(in.CenterKHz, mhzMode)})
		}

		observations = append(observations, rules.Observation{ID: in.ID, CenterKHz: center})
	}
	return observations, errs
}
