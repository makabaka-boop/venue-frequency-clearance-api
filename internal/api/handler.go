// Package api exposes the frequency coordination rules over HTTP via Gin.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"wireless-coordinator/internal/rules"
)

// maxBodyBytes caps the request body; 1 MiB is far more than 200 devices need.
const maxBodyBytes = 1 << 20

// deviceInput is the wire form of one device. Numbers decode as numberInput
// so non-integer values — and string-typed numbers — can be reported against
// the exact field.
type deviceInput struct {
	ID           string      `json:"id"`
	Purpose      string      `json:"purpose"`
	CenterKHz    numberInput `json:"center_khz"`
	BandwidthKHz numberInput `json:"bandwidth_khz"`
}

// numberInput is the wire form of one frequency or bandwidth number. It
// records whether the JSON value arrived as a quoted string: encoding/json
// silently accepts "500000" into a json.Number, but the contract requires an
// actual JSON number, so validation rejects string-typed values against the
// exact field instead of letting them into the interval arithmetic.
type numberInput struct {
	json.Number
	quoted bool
}

// UnmarshalJSON records the number literal, flagging quoted strings so
// validation can reject them as type errors. A missing or null value stays
// empty and is reported by the required-field check, exactly as before.
func (n *numberInput) UnmarshalJSON(b []byte) error {
	n.Number, n.quoted = "", false
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		n.Number, n.quoted = json.Number(s), true
		return nil
	}
	if string(b) != "null" {
		n.Number = json.Number(b)
	}
	return nil
}

type coordinateRequest struct {
	Devices []deviceInput `json:"devices"`
	// IncludeClearance stays raw so a non-boolean value can be reported
	// against the field instead of failing the whole decode.
	IncludeClearance json.RawMessage `json:"include_clearance"`
	// FrequencyUnit stays raw so a non-string or unknown value can be
	// reported against the field instead of failing the whole decode.
	FrequencyUnit json.RawMessage `json:"frequency_unit"`
}

// checkRetunesRequest is the wire form of a retune trial: the existing fleet,
// the one device to retune, and the candidate center frequencies to try.
type checkRetunesRequest struct {
	Devices []deviceInput `json:"devices"`
	// TargetID stays raw so a non-string value can be reported against the
	// field instead of failing the whole decode.
	TargetID            json.RawMessage `json:"target_id"`
	CandidateCentersKHz []numberInput   `json:"candidate_centers_khz"`
	// FrequencyUnit behaves exactly as in coordinateRequest.
	FrequencyUnit json.RawMessage `json:"frequency_unit"`
}

// FrequencyUnitMHz is the only accepted frequency_unit value. With it set,
// center_khz / bandwidth_khz carry MHz numbers with up to three decimal
// places, which the HTTP layer converts to integer kHz before adjudication.
const FrequencyUnitMHz = "mhz"

// fieldError locates one validation problem, e.g. "devices[2].bandwidth_khz".
type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type deviceIntervalJSON struct {
	ID      string `json:"id"`
	LowKHz  int64  `json:"low_khz"`
	HighKHz int64  `json:"high_khz"`
}

type conflictPairJSON struct {
	First  string `json:"first"`
	Second string `json:"second"`
}

// retuneResultJSON is the wire form of one retune trial. OutOfBand and
// Conflicts are present only on rejected trials, mirroring the coordinate
// rejection shape; pointers keep the keys off accepted trials while still
// emitting "[]" for an empty list on rejected ones. Struct field order fixes
// the key order in the emitted JSON.
type retuneResultJSON struct {
	CenterKHz int64                 `json:"center_khz"`
	Accepted  bool                  `json:"accepted"`
	OutOfBand *[]deviceIntervalJSON `json:"out_of_band,omitempty"`
	Conflicts *[]conflictPairJSON   `json:"conflicts,omitempty"`
}

// clearanceJSON is the optional drift-headroom block, present only on
// accepted responses that asked for it. Struct field order fixes the key
// order in the emitted JSON.
type clearanceJSON struct {
	MinimumKHz int64                 `json:"minimum_khz"`
	Devices    []deviceClearanceJSON `json:"devices"`
}

type deviceClearanceJSON struct {
	ID         string `json:"id"`
	MinimumKHz int64  `json:"minimum_khz"`
	Limiter    string `json:"limiter"`
}

// NewRouter builds the Gin engine with all routes.
func NewRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.POST("/v1/coordinate", handleCoordinate)
	r.POST("/v1/check-retunes", handleCheckRetunes)
	r.POST("/v1/reconcile-observations", handleReconcileObservations)
	return r
}

// decodeRequestBody reads the capped body, decodes the single JSON object it
// must contain into req, and rejects duplicated object keys. ok is false when
// an error response has already been written.
func decodeRequestBody(c *gin.Context, req any) bool {
	limited := http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	raw, err := io.ReadAll(limited)
	if err != nil {
		writeErrors(c, http.StatusBadRequest, fieldError{
			Field:   "",
			Message: "invalid JSON body: " + err.Error(),
		})
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(req); err != nil {
		writeErrors(c, http.StatusBadRequest, fieldError{
			Field:   "",
			Message: "invalid JSON body: " + err.Error(),
		})
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErrors(c, http.StatusBadRequest, fieldError{
			Field:   "",
			Message: "body must contain a single JSON object",
		})
		return false
	}

	// Reject duplicated object keys outright: encoding/json would otherwise
	// keep the last occurrence, letting an ambiguous switch, fleet list or
	// device value silently pick the adjudication. These are input-shape
	// errors, reported before semantic validation.
	if dupErrs := duplicateKeyErrors(raw); len(dupErrs) > 0 {
		writeErrors(c, http.StatusBadRequest, dupErrs...)
		return false
	}
	return true
}

func handleCoordinate(c *gin.Context) {
	var req coordinateRequest
	if !decodeRequestBody(c, &req) {
		return
	}

	devices, includeClearance, errs := validateRequest(req)
	if len(errs) > 0 {
		writeErrors(c, http.StatusBadRequest, errs...)
		return
	}

	verdict := rules.Adjudicate(devices)
	if verdict.Accepted {
		resp := gin.H{
			"accepted": true,
			"devices":  toDeviceIntervalsJSON(verdict.Intervals),
		}
		if includeClearance {
			resp["clearance"] = toClearanceJSON(rules.ComputeClearance(verdict.Intervals))
		}
		c.JSON(http.StatusOK, resp)
		return
	}
	// Rejected fleets report only out-of-band and conflict details, even
	// when clearance was requested.
	c.JSON(http.StatusOK, gin.H{
		"accepted":    false,
		"out_of_band": toDeviceIntervalsJSON(verdict.OutOfBand),
		"conflicts":   toConflictsJSON(verdict.Conflicts),
	})
}

// handleCheckRetunes tries each candidate center frequency on the target
// device and reports one trial verdict per candidate, so the coordinator can
// pick a retune value before touching the device list.
func handleCheckRetunes(c *gin.Context) {
	var req checkRetunesRequest
	if !decodeRequestBody(c, &req) {
		return
	}

	devices, targetID, candidates, errs := validateRetuneRequest(req)
	if len(errs) > 0 {
		writeErrors(c, http.StatusBadRequest, errs...)
		return
	}

	trials := rules.AdjudicateRetunes(devices, targetID, candidates)
	results := make([]retuneResultJSON, 0, len(trials))
	for _, t := range trials {
		entry := retuneResultJSON{CenterKHz: t.CenterKHz, Accepted: t.Verdict.Accepted}
		if !t.Verdict.Accepted {
			outOfBand := toDeviceIntervalsJSON(t.Verdict.OutOfBand)
			conflicts := toConflictsJSON(t.Verdict.Conflicts)
			entry.OutOfBand = &outOfBand
			entry.Conflicts = &conflicts
		}
		results = append(results, entry)
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

// validateRetuneRequest checks the retune request shape — frequency unit,
// target selector, fleet and candidate list — collecting one error per
// problem, in the same style and order as validateRequest.
func validateRetuneRequest(req checkRetunesRequest) ([]rules.Device, string, []int64, []fieldError) {
	var errs []fieldError

	inMHz, unitOK := parseFrequencyUnit(req.FrequencyUnit)
	if !unitOK {
		errs = append(errs, fieldError{
			Field:   "frequency_unit",
			Message: fmt.Sprintf(`must be omitted or %q, got %s`, FrequencyUnitMHz, string(req.FrequencyUnit)),
		})
	}

	targetID, targetOK := parseTargetID(req.TargetID)
	if !targetOK {
		errs = append(errs, targetIDProblem(req.TargetID))
	} else if targetID == "" {
		targetOK = false
		errs = append(errs, fieldError{Field: "target_id", Message: "must not be empty"})
	}

	devices, devErrs := validateDevices(req.Devices, inMHz && unitOK)
	errs = append(errs, devErrs...)

	candidates, candErrs := validateCandidates(req.CandidateCentersKHz, inMHz && unitOK)
	errs = append(errs, candErrs...)

	// The target must name one of the submitted devices; the raw inputs are
	// consulted so the check still runs when other device fields are invalid.
	if targetOK {
		found := false
		for _, in := range req.Devices {
			if in.ID == targetID {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, fieldError{
				Field:   "target_id",
				Message: fmt.Sprintf("no device has id %q", targetID),
			})
		}
	}
	return devices, targetID, candidates, errs
}

// parseTargetID reads the required target_id selector. It must be a JSON
// string; absent, null and non-string values are reported against the field.
func parseTargetID(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func targetIDProblem(raw json.RawMessage) fieldError {
	if len(raw) == 0 {
		return fieldError{Field: "target_id", Message: "is required"}
	}
	return fieldError{Field: "target_id", Message: fmt.Sprintf("must be a string, got %s", string(raw))}
}

// validateCandidates checks the candidate list size and converts every
// candidate to integer kHz exactly like a device center_khz. Candidates that
// normalize to the same kHz value are ambiguous — the trial would run twice
// on identical input — and are rejected against the later occurrence.
func validateCandidates(inputs []numberInput, mhzMode bool) ([]int64, []fieldError) {
	var errs []fieldError

	switch {
	case inputs == nil:
		return nil, []fieldError{{Field: "candidate_centers_khz", Message: "is required"}}
	case len(inputs) < rules.MinCandidates || len(inputs) > rules.MaxCandidates:
		errs = append(errs, fieldError{
			Field:   "candidate_centers_khz",
			Message: fmt.Sprintf("must contain between %d and %d candidates, got %d", rules.MinCandidates, rules.MaxCandidates, len(inputs)),
		})
	}

	centers := make([]int64, 0, len(inputs))
	seen := make(map[int64]int, len(inputs))
	for i, n := range inputs {
		field := fmt.Sprintf("candidate_centers_khz[%d]", i)
		center, ok := parseFrequency(n, mhzMode)
		if !ok {
			errs = append(errs, fieldError{Field: field, Message: frequencyProblem(n, mhzMode)})
			continue
		}
		if prev, dup := seen[center]; dup {
			errs = append(errs, fieldError{
				Field:   field,
				Message: fmt.Sprintf("duplicates candidate_centers_khz[%d]: both normalize to %d kHz", prev, center),
			})
			continue
		}
		seen[center] = i
		centers = append(centers, center)
	}
	return centers, errs
}

// validateRequest checks the fleet and every device field, collecting one
// error per problem so the coordinator can fix everything in one pass.
func validateRequest(req coordinateRequest) ([]rules.Device, bool, []fieldError) {
	var errs []fieldError

	includeClearance, ok := parseIncludeClearance(req.IncludeClearance)
	if !ok {
		errs = append(errs, fieldError{
			Field:   "include_clearance",
			Message: fmt.Sprintf("must be a boolean, got %s", string(req.IncludeClearance)),
		})
	}

	// frequency_unit is a top-level input-shape choice: anything other than
	// absent or "mhz" (an explicit "khz", unknown units, null or another
	// type) is rejected before adjudication. With an invalid unit the device
	// numbers keep their legacy kHz parsing, so spurious precision errors do
	// not mask the actual unit mistake.
	inMHz, unitOK := parseFrequencyUnit(req.FrequencyUnit)
	if !unitOK {
		errs = append(errs, fieldError{
			Field:   "frequency_unit",
			Message: fmt.Sprintf(`must be omitted or %q, got %s`, FrequencyUnitMHz, string(req.FrequencyUnit)),
		})
	}

	devices, devErrs := validateDevices(req.Devices, inMHz && unitOK)
	errs = append(errs, devErrs...)
	return devices, includeClearance, errs
}

// validateDevices checks the fleet size and every device field, collecting
// one error per problem so the coordinator can fix everything in one pass.
// mhzMode reports whether center_khz / bandwidth_khz carry MHz numbers.
func validateDevices(inputs []deviceInput, mhzMode bool) ([]rules.Device, []fieldError) {
	var errs []fieldError

	switch {
	case inputs == nil:
		return nil, []fieldError{{Field: "devices", Message: "is required"}}
	case len(inputs) < rules.MinDevices || len(inputs) > rules.MaxDevices:
		errs = append(errs, fieldError{
			Field:   "devices",
			Message: fmt.Sprintf("must contain between %d and %d devices, got %d", rules.MinDevices, rules.MaxDevices, len(inputs)),
		})
	}

	devices := make([]rules.Device, 0, len(inputs))
	seen := make(map[string]int, len(inputs))
	for i, in := range inputs {
		prefix := fmt.Sprintf("devices[%d]", i)

		if in.ID == "" {
			errs = append(errs, fieldError{Field: prefix + ".id", Message: "must not be empty"})
		} else if prev, dup := seen[in.ID]; dup {
			errs = append(errs, fieldError{
				Field:   prefix + ".id",
				Message: fmt.Sprintf("duplicates devices[%d].id %q", prev, in.ID),
			})
		} else {
			seen[in.ID] = i
		}

		if _, ok := rules.GuardKHz(in.Purpose); !ok {
			errs = append(errs, fieldError{
				Field:   prefix + ".purpose",
				Message: `must be one of "handheld", "bodypack", "ifb"`,
			})
		}

		center, centerOK := parseFrequency(in.CenterKHz, mhzMode)
		if !centerOK {
			errs = append(errs, fieldError{Field: prefix + ".center_khz", Message: frequencyProblem(in.CenterKHz, mhzMode)})
		}

		// The bandwidth range check is unchanged across units: the MHz value
		// is converted to integer kHz first, then the existing 25..400 kHz
		// range applies, with no unit-specific branch.
		bw, bwOK := parseFrequency(in.BandwidthKHz, mhzMode)
		if !bwOK {
			errs = append(errs, fieldError{Field: prefix + ".bandwidth_khz", Message: frequencyProblem(in.BandwidthKHz, mhzMode)})
		} else if bw < rules.MinBandwidthKHz || bw > rules.MaxBandwidthKHz {
			errs = append(errs, fieldError{
				Field:   prefix + ".bandwidth_khz",
				Message: fmt.Sprintf("must be between %d and %d kHz, got %d", rules.MinBandwidthKHz, rules.MaxBandwidthKHz, bw),
			})
		}

		devices = append(devices, rules.Device{
			ID:           in.ID,
			Purpose:      in.Purpose,
			CenterKHz:    center,
			BandwidthKHz: bw,
		})
	}
	return devices, errs
}

// parseFrequencyUnit reads the optional frequency_unit selector. Absent means
// legacy integer-kHz mode; the only accepted value is the JSON string "mhz".
// Anything else — null, numbers, objects or an unknown unit such as "khz" —
// must be rejected before adjudication.
func parseFrequencyUnit(raw json.RawMessage) (mhz bool, ok bool) {
	if len(raw) == 0 {
		return false, true
	}
	if string(raw) == "null" {
		return false, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false, false
	}
	return s == FrequencyUnitMHz, s == FrequencyUnitMHz
}

// parseIncludeClearance reads the optional include_clearance switch. Absent
// means false; anything present that is not a JSON boolean — null included —
// is a type error.
func parseIncludeClearance(raw json.RawMessage) (value, ok bool) {
	if len(raw) == 0 {
		return false, true
	}
	// json.Unmarshal accepts null into a bool without error; reject it
	// explicitly so only true/false are valid switch values.
	if string(raw) == "null" {
		return false, false
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}

// parseFrequency converts a JSON number to an integer kHz value. When mhz is
// true the number is interpreted as MHz with up to three decimal places and
// converted by decimal fixed-point arithmetic (value * 1000); otherwise only
// an integer number of kHz is accepted. A string-typed value is never
// accepted, whatever the unit: the field requires a JSON number.
func parseFrequency(n numberInput, mhz bool) (int64, bool) {
	if n.quoted || n.String() == "" {
		return 0, false
	}
	if mhz {
		v, reason := parseMHzToKHz(n.String())
		return v, reason == mhzOK
	}
	v, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return v, true
}

func frequencyProblem(n numberInput, mhz bool) string {
	if n.quoted {
		return quotedNumberProblem(n.String(), mhz)
	}
	if mhz {
		return mhzProblem(n.String(), parseMHzProblem(n.String()))
	}
	return kHzProblem(n.Number)
}

// quotedNumberProblem reports a string-typed number such as "500000" where
// the contract requires a JSON number. The got-value keeps its quotes so the
// type mistake stays visible in the message.
func quotedNumberProblem(s string, mhz bool) string {
	if mhz {
		return fmt.Sprintf("must be a MHz number with at most three decimal places converting to an integer kHz, got %q", s)
	}
	return fmt.Sprintf("must be an integer number of kHz, got %q", s)
}

func kHzProblem(n json.Number) string {
	if n.String() == "" {
		return "is required and must be an integer number of kHz"
	}
	return fmt.Sprintf("must be an integer number of kHz, got %s", n.String())
}

// MHz-to-kHz conversion failure reasons.
const (
	mhzOK            = ""
	mhzMissing       = "missing"
	mhzBadPrecision  = "precision"
	mhzOutOfIntRange = "range"
)

// parseMHzProblem reports why a JSON number is not an accepted MHz value; it
// returns mhzOK when the value converts to an integer kHz exactly.
func parseMHzProblem(s string) string {
	_, reason := parseMHzToKHz(s)
	return reason
}

func mhzProblem(s, reason string) string {
	switch reason {
	case mhzMissing:
		return "is required and must be a MHz number with at most three decimal places"
	case mhzOutOfIntRange:
		return fmt.Sprintf("must convert to an integer kHz inside the int64 range, got %s MHz", s)
	default:
		return fmt.Sprintf("must be a MHz number with at most three decimal places converting to an integer kHz, got %s", s)
	}
}

// parseMHzToKHz converts a decimal fixed-point JSON number in MHz to an exact
// integer kHz by multiplying by 1000, without going through a floating-point
// value. Accepted grammar: an optional sign, one or more integer digits, an
// optional decimal point followed by 1 to 3 digits. Exponent notation is not
// accepted (its printed precision is ambiguous), and a value that would not
// fit in int64 kHz is rejected. The JSON grammar excludes lone ".5" / "5.",
// but they are rejected here as well rather than trusted.
func parseMHzToKHz(s string) (int64, string) {
	if s == "" {
		return 0, mhzMissing
	}
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, mhzBadPrecision
		}
	}
	if !allDigits(intPart) {
		return 0, mhzBadPrecision
	}
	// A decimal point must carry fractional digits (JSON numbers never end
	// with one). At most three fractional digits are allowed: kHz is the
	// finest resolution, so a fourth decimal place would be a sub-kHz
	// fraction with no integer conversion.
	if strings.ContainsRune(s, '.') && !allDigits(fracPart) {
		return 0, mhzBadPrecision
	}
	if len(fracPart) > 3 {
		return 0, mhzBadPrecision
	}

	// kHz value as a sign-free digit string: integer MHz * 1000 plus the
	// fractional MHz padded to three digits (0.201 MHz -> 201 kHz).
	khzDigits := strings.TrimLeft(intPart+fracPart+strings.Repeat("0", 3-len(fracPart)), "0")
	if khzDigits == "" {
		return 0, mhzOK // ±0
	}
	limit := "9223372036854775808" // |math.MinInt64|, one above MaxInt64
	tooLarge := len(khzDigits) > len(limit) ||
		(len(khzDigits) == len(limit) && khzDigits > limit)
	equalMin := khzDigits == limit
	// MaxInt64 kHz is limit-1, so positive values may not reach limit;
	// negative ones may equal it but not exceed it.
	if tooLarge || (!neg && equalMin) {
		return 0, mhzOutOfIntRange
	}
	if neg {
		// Accumulate in the negative direction so -2^63 fits without an
		// intermediate overflow.
		var v int64
		for _, d := range khzDigits {
			v = v*10 - int64(d-'0')
		}
		return v, mhzOK
	}
	var v int64
	for _, d := range khzDigits {
		v = v*10 + int64(d-'0')
	}
	return v, mhzOK
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func writeErrors(c *gin.Context, status int, errs ...fieldError) {
	c.JSON(status, gin.H{"accepted": false, "errors": errs})
}

func toDeviceIntervalsJSON(dis []rules.DeviceInterval) []deviceIntervalJSON {
	out := make([]deviceIntervalJSON, 0, len(dis))
	for _, di := range dis {
		out = append(out, deviceIntervalJSON{
			ID:      di.ID,
			LowKHz:  di.Interval.LowKHz,
			HighKHz: di.Interval.HighKHz,
		})
	}
	return out
}

func toConflictsJSON(pairs []rules.ConflictPair) []conflictPairJSON {
	out := make([]conflictPairJSON, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, conflictPairJSON{First: p.First, Second: p.Second})
	}
	return out
}

func toClearanceJSON(cl rules.Clearance) clearanceJSON {
	out := clearanceJSON{
		MinimumKHz: cl.MinimumKHz,
		Devices:    make([]deviceClearanceJSON, 0, len(cl.Devices)),
	}
	for _, d := range cl.Devices {
		out.Devices = append(out.Devices, deviceClearanceJSON{
			ID:         d.ID,
			MinimumKHz: d.MinimumKHz,
			Limiter:    d.Limiter,
		})
	}
	return out
}
