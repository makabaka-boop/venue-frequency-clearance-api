// Package rules implements the frequency coordination rules for wireless
// microphones sharing one venue across multiple conferences. It is a pure
// domain package: no HTTP, no JSON, no I/O.
package rules

import (
	"math"
	"sort"
)

// Allowed band, closed interval, in kHz.
const (
	BandLowKHz  int64 = 470000
	BandHighKHz int64 = 694000
)

// Bandwidth limits, in kHz.
const (
	MinBandwidthKHz int64 = 25
	MaxBandwidthKHz int64 = 400
)

// Fleet size limits.
const (
	MinDevices = 1
	MaxDevices = 200
)

// Supported purposes.
const (
	PurposeHandheld = "handheld"
	PurposeBodypack = "bodypack"
	PurposeIFB      = "ifb"
)

// guardByPurpose maps a purpose to its fixed per-side guard interval in kHz.
var guardByPurpose = map[string]int64{
	PurposeHandheld: 125,
	PurposeBodypack: 175,
	PurposeIFB:      250,
}

// GuardKHz returns the per-side guard interval for a purpose.
// ok is false when the purpose is unknown.
func GuardKHz(purpose string) (int64, bool) {
	g, ok := guardByPurpose[purpose]
	return g, ok
}

// Device is one wireless microphone to coordinate.
type Device struct {
	ID           string
	Purpose      string
	CenterKHz    int64
	BandwidthKHz int64
}

// Interval is a closed interval [LowKHz, HighKHz] in kHz.
type Interval struct {
	LowKHz  int64
	HighKHz int64
}

// ProtectedInterval computes the protected interval of a device: the
// occupied interval [center-floor(bw/2), center+ceil(bw/2)] extended by the
// purpose guard interval on both sides. ok is false for an unknown purpose.
//
// Arithmetic saturates at the int64 limits, so an extreme center frequency
// yields a huge but well-ordered interval that InBand rejects, instead of
// wrapping around into an inverted, seemingly in-band one.
func ProtectedInterval(d Device) (Interval, bool) {
	guard, ok := GuardKHz(d.Purpose)
	if !ok {
		return Interval{}, false
	}
	halfFloor := d.BandwidthKHz / 2
	halfCeil := d.BandwidthKHz/2 + d.BandwidthKHz%2 // ceil(bw/2) that cannot overflow
	return Interval{
		LowKHz:  subSat(subSat(d.CenterKHz, halfFloor), guard),
		HighKHz: addSat(addSat(d.CenterKHz, halfCeil), guard),
	}, true
}

// addSat returns a + b, saturating to the int64 limits on overflow.
func addSat(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

// subSat returns a - b, saturating to the int64 limits on overflow.
func subSat(a, b int64) int64 {
	if b < 0 && a > math.MaxInt64+b {
		return math.MaxInt64
	}
	if b > 0 && a < math.MinInt64+b {
		return math.MinInt64
	}
	return a - b
}

// InBand reports whether the interval lies fully inside the allowed band
// [BandLowKHz, BandHighKHz]. Endpoints exactly on the band edge are inside.
func InBand(iv Interval) bool {
	return iv.LowKHz >= BandLowKHz && iv.HighKHz <= BandHighKHz
}

// Overlaps reports whether two closed intervals intersect. Touching
// endpoints count as a conflict.
func Overlaps(a, b Interval) bool {
	return a.LowKHz <= b.HighKHz && b.LowKHz <= a.HighKHz
}

// DeviceInterval pairs a device ID with its protected interval.
type DeviceInterval struct {
	ID       string
	Interval Interval
}

// ConflictPair is an unordered pair of conflicting device IDs, normalized
// so that First < Second in lexicographic order.
type ConflictPair struct {
	First  string
	Second string
}

// Verdict is the single outcome of adjudicating a fleet.
type Verdict struct {
	Accepted  bool
	Intervals []DeviceInterval // every device, sorted by ID
	OutOfBand []DeviceInterval // devices whose protected interval leaves the band, sorted by ID
	Conflicts []ConflictPair   // sorted by (First, Second), deduplicated
}

// Adjudicate evaluates a fleet of devices and returns the single verdict.
// The fleet is rejected as a whole when any protected interval leaves the
// allowed band or any two protected intervals overlap. Conflicts are
// reported over all devices, including out-of-band ones, so the coordinator
// can review every problem at once.
func Adjudicate(devices []Device) Verdict {
	intervals := make([]DeviceInterval, 0, len(devices))
	outOfBand := make([]DeviceInterval, 0)
	for _, d := range devices {
		iv, ok := ProtectedInterval(d)
		if !ok {
			// Unknown purpose is a validation concern, handled before
			// adjudication; skip it here rather than invent an interval.
			continue
		}
		di := DeviceInterval{ID: d.ID, Interval: iv}
		intervals = append(intervals, di)
		if !InBand(iv) {
			outOfBand = append(outOfBand, di)
		}
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].ID < intervals[j].ID })
	sort.Slice(outOfBand, func(i, j int) bool { return outOfBand[i].ID < outOfBand[j].ID })

	conflicts := findConflicts(intervals)

	return Verdict{
		Accepted:  len(outOfBand) == 0 && len(conflicts) == 0,
		Intervals: intervals,
		OutOfBand: outOfBand,
		Conflicts: conflicts,
	}
}

// findConflicts returns every conflicting pair over the given device
// intervals. Each pair is normalized (First < Second), and the list is
// sorted by First then Second and deduplicated.
func findConflicts(intervals []DeviceInterval) []ConflictPair {
	pairs := make([]ConflictPair, 0)
	for i := 0; i < len(intervals); i++ {
		for j := i + 1; j < len(intervals); j++ {
			if !Overlaps(intervals[i].Interval, intervals[j].Interval) {
				continue
			}
			first, second := intervals[i].ID, intervals[j].ID
			if second < first {
				first, second = second, first
			}
			pairs = append(pairs, ConflictPair{First: first, Second: second})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].First != pairs[j].First {
			return pairs[i].First < pairs[j].First
		}
		return pairs[i].Second < pairs[j].Second
	})
	// Defensive dedupe: with unique device IDs each pair occurs once.
	deduped := pairs[:0]
	for i, p := range pairs {
		if i > 0 && p == pairs[i-1] {
			continue
		}
		deduped = append(deduped, p)
	}
	return deduped
}
