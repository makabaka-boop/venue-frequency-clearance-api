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

// deviceInput is the wire form of one device. Numbers decode as json.Number
// so non-integer values can be reported against the exact field.
type deviceInput struct {
	ID           string      `json:"id"`
	Purpose      string      `json:"purpose"`
	CenterKHz    json.Number `json:"center_khz"`
	BandwidthKHz json.Number `json:"bandwidth_khz"`
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
	return r
}

func handleCoordinate(c *gin.Context) {
	var req coordinateRequest
	limited := http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	raw, err := io.ReadAll(limited)
	if err != nil {
		writeErrors(c, http.StatusBadRequest, fieldError{
			Field:   "",
			Message: "invalid JSON body: " + err.Error(),
		})
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErrors(c, http.StatusBadRequest, fieldError{
			Field:   "",
			Message: "invalid JSON body: " + err.Error(),
		})
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErrors(c, http.StatusBadRequest, fieldError{
			Field:   "",
			Message: "body must contain a single JSON object",
		})
		return
	}

	// Reject duplicated object keys outright: encoding/json would otherwise
	// keep the last occurrence, letting an ambiguous switch, fleet list or
	// device value silently pick the adjudication. These are input-shape
	// errors, reported before semantic validation.
	if dupErrs := duplicateKeyErrors(raw); len(dupErrs) > 0 {
		writeErrors(c, http.StatusBadRequest, dupErrs...)
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

	switch {
	case req.Devices == nil:
		errs = append(errs, fieldError{Field: "devices", Message: "is required"})
		return nil, includeClearance, errs
	case len(req.Devices) < rules.MinDevices || len(req.Devices) > rules.MaxDevices:
		errs = append(errs, fieldError{
			Field:   "devices",
			Message: fmt.Sprintf("must contain between %d and %d devices, got %d", rules.MinDevices, rules.MaxDevices, len(req.Devices)),
		})
	}

	devices := make([]rules.Device, 0, len(req.Devices))
	seen := make(map[string]int, len(req.Devices))
	for i, in := range req.Devices {
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

		center, centerOK := parseFrequency(in.CenterKHz, inMHz && unitOK)
		if !centerOK {
			errs = append(errs, fieldError{Field: prefix + ".center_khz", Message: frequencyProblem(in.CenterKHz, inMHz && unitOK)})
		}

		// The bandwidth range check is unchanged across units: the MHz value
		// is converted to integer kHz first, then the existing 25..400 kHz
		// range applies, with no unit-specific branch.
		bw, bwOK := parseFrequency(in.BandwidthKHz, inMHz && unitOK)
		if !bwOK {
			errs = append(errs, fieldError{Field: prefix + ".bandwidth_khz", Message: frequencyProblem(in.BandwidthKHz, inMHz && unitOK)})
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
	return devices, includeClearance, errs
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
// an integer number of kHz is accepted.
func parseFrequency(n json.Number, mhz bool) (int64, bool) {
	if n.String() == "" {
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

func frequencyProblem(n json.Number, mhz bool) string {
	if mhz {
		return mhzProblem(n.String(), parseMHzProblem(n.String()))
	}
	return kHzProblem(n)
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
