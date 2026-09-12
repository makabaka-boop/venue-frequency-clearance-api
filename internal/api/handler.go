// Package api exposes the frequency coordination rules over HTTP via Gin.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

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
}

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
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes))
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

	devices, errs := validateRequest(req)
	if len(errs) > 0 {
		writeErrors(c, http.StatusBadRequest, errs...)
		return
	}

	verdict := rules.Adjudicate(devices)
	if verdict.Accepted {
		c.JSON(http.StatusOK, gin.H{
			"accepted": true,
			"devices":  toDeviceIntervalsJSON(verdict.Intervals),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"accepted":    false,
		"out_of_band": toDeviceIntervalsJSON(verdict.OutOfBand),
		"conflicts":   toConflictsJSON(verdict.Conflicts),
	})
}

// validateRequest checks the fleet and every device field, collecting one
// error per problem so the coordinator can fix everything in one pass.
func validateRequest(req coordinateRequest) ([]rules.Device, []fieldError) {
	var errs []fieldError

	switch {
	case req.Devices == nil:
		errs = append(errs, fieldError{Field: "devices", Message: "is required"})
		return nil, errs
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

		center, ok := parseKHz(in.CenterKHz)
		if !ok {
			errs = append(errs, fieldError{Field: prefix + ".center_khz", Message: kHzProblem(in.CenterKHz)})
		}

		bw, ok := parseKHz(in.BandwidthKHz)
		if !ok {
			errs = append(errs, fieldError{Field: prefix + ".bandwidth_khz", Message: kHzProblem(in.BandwidthKHz)})
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

// parseKHz converts a JSON number to an integer kHz value.
func parseKHz(n json.Number) (int64, bool) {
	if n.String() == "" {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return v, true
}

func kHzProblem(n json.Number) string {
	if n.String() == "" {
		return "is required and must be an integer number of kHz"
	}
	return fmt.Sprintf("must be an integer number of kHz, got %s", n.String())
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
