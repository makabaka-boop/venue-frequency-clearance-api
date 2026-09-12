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

// Candidate center frequency limits for one retune trial request.
const (
	MinCandidates = 1
	MaxCandidates = 50
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

// Limiter identifiers for the band endpoints, used in DeviceClearance.Limiter
// when the tightest constraint is a band edge rather than a neighbor device.
const (
	LimiterBandLow  = "band_low"
	LimiterBandHigh = "band_high"
)

// DeviceClearance is the frequency-drift headroom of one device: the kHz
// distance to the tightest constraint, and which constraint sets it.
type DeviceClearance struct {
	ID         string
	MinimumKHz int64
	Limiter    string // LimiterBandLow, LimiterBandHigh, or a neighbor's device ID
}

// Clearance is the drift headroom of a fleet: the global minimum plus one
// entry per device, sorted by device ID.
type Clearance struct {
	MinimumKHz int64
	Devices    []DeviceClearance
}

// clearanceCandidate is one constraint on one device: a margin in kHz and
// the identifier of what sets it.
type clearanceCandidate struct {
	value   int64
	limiter string
}

// tighterCandidate picks the candidate with the smaller margin; equal
// margins go to the lexicographically smaller limiter identifier, so the
// choice never depends on evaluation order.
func tighterCandidate(a, b clearanceCandidate) clearanceCandidate {
	if b.value < a.value || (b.value == a.value && b.limiter < a.limiter) {
		return b
	}
	return a
}

// ComputeClearance measures how far the protected intervals can drift before
// the verdict would change. It is meaningful for accepted fleets, where every
// interval lies inside the band and no two overlap.
//
// The band margin of a device is the distance from its protected interval to
// each band endpoint: low_khz - BandLowKHz and BandHighKHz - high_khz. The
// neighbor margin is the number of unoccupied integer kHz ticks between two
// adjacent protected intervals: right.LowKHz - high_khz - 1. Each device
// reports the tightest of its candidates; ties go to the lexicographically
// smallest limiter identifier ("band_high" < "band_low", neighbor IDs
// compared as plain strings).
//
// All arithmetic saturates at the int64 limits, so even intervals derived
// from extreme center frequencies yield well-ordered margins instead of
// wrapping around.
func ComputeClearance(intervals []DeviceInterval) Clearance {
	byFreq := make([]DeviceInterval, len(intervals))
	copy(byFreq, intervals)
	sort.Slice(byFreq, func(i, j int) bool {
		if byFreq[i].Interval.LowKHz != byFreq[j].Interval.LowKHz {
			return byFreq[i].Interval.LowKHz < byFreq[j].Interval.LowKHz
		}
		return byFreq[i].ID < byFreq[j].ID
	})

	devices := make([]DeviceClearance, 0, len(byFreq))
	for i, di := range byFreq {
		iv := di.Interval
		best := tighterCandidate(
			clearanceCandidate{value: subSat(iv.LowKHz, BandLowKHz), limiter: LimiterBandLow},
			clearanceCandidate{value: subSat(BandHighKHz, iv.HighKHz), limiter: LimiterBandHigh},
		)
		if i > 0 {
			best = tighterCandidate(best, clearanceCandidate{
				value:   subSat(subSat(iv.LowKHz, byFreq[i-1].Interval.HighKHz), 1),
				limiter: byFreq[i-1].ID,
			})
		}
		if i < len(byFreq)-1 {
			best = tighterCandidate(best, clearanceCandidate{
				value:   subSat(subSat(byFreq[i+1].Interval.LowKHz, iv.HighKHz), 1),
				limiter: byFreq[i+1].ID,
			})
		}
		devices = append(devices, DeviceClearance{ID: di.ID, MinimumKHz: best.value, Limiter: best.limiter})
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })

	clearance := Clearance{Devices: devices}
	if len(devices) > 0 {
		clearance.MinimumKHz = devices[0].MinimumKHz
		for _, d := range devices[1:] {
			if d.MinimumKHz < clearance.MinimumKHz {
				clearance.MinimumKHz = d.MinimumKHz
			}
		}
	}
	return clearance
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

// RetuneVerdict is the trial verdict for one candidate center frequency: the
// fleet adjudicated with the target device retuned to CenterKHz.
type RetuneVerdict struct {
	CenterKHz int64
	Verdict   Verdict
}

// AdjudicateRetunes tries each candidate center frequency for one target
// device: the candidate replaces the target's center, every other device and
// the target's purpose and bandwidth stay untouched, and the modified fleet
// is adjudicated exactly as a submitted one. The input slices are not
// modified.
//
// Results are sorted by candidate center frequency ascending, so the report
// is stable no matter what order the candidates were submitted in.
func AdjudicateRetunes(devices []Device, targetID string, candidates []int64) []RetuneVerdict {
	results := make([]RetuneVerdict, 0, len(candidates))
	for _, center := range candidates {
		trial := make([]Device, len(devices))
		copy(trial, devices)
		for i := range trial {
			if trial[i].ID == targetID {
				trial[i].CenterKHz = center
			}
		}
		results = append(results, RetuneVerdict{CenterKHz: center, Verdict: Adjudicate(trial)})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].CenterKHz < results[j].CenterKHz })
	return results
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
