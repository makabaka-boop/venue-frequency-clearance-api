package rules

import (
	"math"
	"sort"
)

// Observation and reconciliation request size limits.
const (
	MinObservations = 1
	MaxObservations = 500
)

// Matching tolerance limits, in kHz.
const (
	MinToleranceKHz int64 = 0
	MaxToleranceKHz int64 = 10000
)

// ReconDevice is one planned device as seen by the post-show reconciliation:
// only its id and planned carrier center matter — the spectrum analyzer
// records carriers, not guard intervals.
type ReconDevice struct {
	ID        string
	CenterKHz int64
}

// Observation is one carrier the analyzer recorded on site: a unique id and
// the measured center frequency in kHz.
type Observation struct {
	ID        string
	CenterKHz int64
}

// MatchedObservation pairs one device with the observation occupied by it.
// DeviationKHz is the signed difference observed - planned: positive means
// the measured carrier sits above the planned center, negative below.
type MatchedObservation struct {
	DeviceID       string
	ObservationID  string
	ObservedCenter int64
	DeviationKHz   int64
}

// Reconciliation is the outcome of checking on-site observations against the
// device plan. It is not an adjudication: every valid device and observation
// appears exactly once across Matched, Missing and Unexpected.
type Reconciliation struct {
	Matched    []MatchedObservation // per device, sorted by device id
	Missing    []string             // ids of devices no observation was occupied for
	Unexpected []Observation        // observations no device was occupied by
}

// reconcileCandidate is one device/observation pair whose frequencies fall
// within the tolerance.
type reconcileCandidate struct {
	deviceIndex int
	obsIndex    int
	diff        int64 // |observed - planned|
	deviation   int64 // observed - planned
}

// ReconcileObservations matches on-site carrier observations to the planned
// devices without re-running the release adjudication: the only question is
// which planned device each recorded carrier belongs to (and vice versa).
//
// Every device/observation pair whose absolute frequency difference is at
// most toleranceKHz is a candidate. Candidates are occupied greedily in the
// total order (absolute frequency difference, device id, observation id), so
// the closest pair is taken first and each device and observation is occupied
// at most once: the result is one-to-one and independent of the order the
// devices and observations were submitted in. Devices left unoccupied are
// missing (never switched on); observations left unoccupied are unexpected
// (switched on without a plan, or on the wrong frequency).
//
// The difference arithmetic saturates at the int64 limits, so extreme center
// frequencies compare by a capped distance instead of wrapping around into a
// small difference.
func ReconcileObservations(devices []ReconDevice, observations []Observation, toleranceKHz int64) Reconciliation {
	candidates := make([]reconcileCandidate, 0)
	for di, d := range devices {
		for oi, o := range observations {
			diff, deviation := frequencyDistance(d.CenterKHz, o.CenterKHz)
			if diff > toleranceKHz {
				continue
			}
			candidates = append(candidates, reconcileCandidate{
				deviceIndex: di,
				obsIndex:    oi,
				diff:        diff,
				deviation:   deviation,
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.diff != b.diff {
			return a.diff < b.diff
		}
		if da, db := devices[a.deviceIndex].ID, devices[b.deviceIndex].ID; da != db {
			return da < db
		}
		return observations[a.obsIndex].ID < observations[b.obsIndex].ID
	})

	deviceTaken := make([]bool, len(devices))
	obsTaken := make([]bool, len(observations))
	matched := make([]MatchedObservation, 0)
	for _, cand := range candidates {
		if deviceTaken[cand.deviceIndex] || obsTaken[cand.obsIndex] {
			continue
		}
		deviceTaken[cand.deviceIndex] = true
		obsTaken[cand.obsIndex] = true
		d, o := devices[cand.deviceIndex], observations[cand.obsIndex]
		matched = append(matched, MatchedObservation{
			DeviceID:       d.ID,
			ObservationID:  o.ID,
			ObservedCenter: o.CenterKHz,
			DeviationKHz:   cand.deviation,
		})
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].DeviceID < matched[j].DeviceID })

	missing := make([]string, 0)
	for i, taken := range deviceTaken {
		if !taken {
			missing = append(missing, devices[i].ID)
		}
	}
	sort.Strings(missing)

	unexpected := make([]Observation, 0)
	for i, taken := range obsTaken {
		if !taken {
			unexpected = append(unexpected, observations[i])
		}
	}
	sort.Slice(unexpected, func(i, j int) bool { return unexpected[i].ID < unexpected[j].ID })

	return Reconciliation{Matched: matched, Missing: missing, Unexpected: unexpected}
}

// frequencyDistance returns (|observed - planned|, observed - planned) with
// saturating arithmetic, so extreme centers yield a capped distance instead
// of a wrapped one. The signed deviation uses the same saturated subtraction
// as the interval arithmetic elsewhere in the package.
func frequencyDistance(planned, observed int64) (abs int64, signed int64) {
	signed = subSat(observed, planned)
	abs = signed
	if abs < 0 {
		abs = -abs
		if abs < 0 { // MinInt64 negation overflow
			abs = math.MaxInt64
		}
	}
	return abs, signed
}
