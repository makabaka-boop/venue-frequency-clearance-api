package rules

import (
	"math"
	"reflect"
	"testing"
)

func TestReconcileObservationsShuffledOneToOne(t *testing.T) {
	// Planned centers: a 500000, b 500050, c 600000, d 650000.
	// Analyzer carriers: obs-1 at 500010 (a +10), obs-2 at 500040 (b -10),
	// obs-3 at 600000 (c), obs-ghost at 550000 (nothing planned there).
	devices := []ReconDevice{
		{ID: "d", CenterKHz: 650000},
		{ID: "b", CenterKHz: 500050},
		{ID: "a", CenterKHz: 500000},
		{ID: "c", CenterKHz: 600000},
	}
	observations := []Observation{
		{ID: "obs-2", CenterKHz: 500040},
		{ID: "obs-ghost", CenterKHz: 550000},
		{ID: "obs-3", CenterKHz: 600000},
		{ID: "obs-1", CenterKHz: 500010},
	}
	want := Reconciliation{
		Matched: []MatchedObservation{
			{DeviceID: "a", ObservationID: "obs-1", ObservedCenter: 500010, DeviationKHz: 10},
			{DeviceID: "b", ObservationID: "obs-2", ObservedCenter: 500040, DeviationKHz: -10},
			{DeviceID: "c", ObservationID: "obs-3", ObservedCenter: 600000, DeviationKHz: 0},
		},
		Missing: []string{"d"},
		Unexpected: []Observation{
			{ID: "obs-ghost", CenterKHz: 550000},
		},
	}

	got := ReconcileObservations(devices, observations, 100)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reconciliation = %+v; want %+v", got, want)
	}

	// Reordering both inputs independently must give the identical result.
	shuffledDevices := []ReconDevice{
		{ID: "c", CenterKHz: 600000},
		{ID: "a", CenterKHz: 500000},
		{ID: "d", CenterKHz: 650000},
		{ID: "b", CenterKHz: 500050},
	}
	shuffledObs := []Observation{
		{ID: "obs-3", CenterKHz: 600000},
		{ID: "obs-1", CenterKHz: 500010},
		{ID: "obs-ghost", CenterKHz: 550000},
		{ID: "obs-2", CenterKHz: 500040},
	}
	if got2 := ReconcileObservations(shuffledDevices, shuffledObs, 100); !reflect.DeepEqual(got2, want) {
		t.Fatalf("shuffled reconciliation = %+v; want %+v", got2, want)
	}
}

// Two devices planned at the same center and two equidistant observations are
// an exact tie at every numeric key: greedy occupation must fall back to
// (device id, observation id) so the pairing is one-to-one and deterministic.
func TestReconcileObservationsDeterministicTieBreak(t *testing.T) {
	devices := []ReconDevice{
		{ID: "b", CenterKHz: 500000},
		{ID: "a", CenterKHz: 500000},
	}
	observations := []Observation{
		{ID: "o2", CenterKHz: 500005},
		{ID: "o1", CenterKHz: 500005},
	}
	want := Reconciliation{
		Matched: []MatchedObservation{
			{DeviceID: "a", ObservationID: "o1", ObservedCenter: 500005, DeviationKHz: 5},
			{DeviceID: "b", ObservationID: "o2", ObservedCenter: 500005, DeviationKHz: 5},
		},
		Missing:    []string{},
		Unexpected: []Observation{},
	}
	if got := ReconcileObservations(devices, observations, 10); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v; want %+v", got, want)
	}

	// Swap the observation submission order: output must not change.
	reversed := []Observation{observations[1], observations[0]}
	if got := ReconcileObservations(devices, reversed, 10); !reflect.DeepEqual(got, want) {
		t.Fatalf("reversed observations: got %+v; want %+v", got, want)
	}
}

// The closest pair is occupied first, so a carrier that could match two
// devices is assigned to the nearer one; the displaced device goes missing
// and the other carrier becomes unexpected.
func TestReconcileObservationsClosestPairOccupiedFirst(t *testing.T) {
	// a at 500000, b at 500010. Observation o-near at 500009 is within
	// tolerance of both (diff 1 to b, diff 9 to a); o-far at 499995 is only
	// within tolerance of a (diff 5). Occupation order: (1,b,o-near) first,
	// then (5,a,o-far), so every device matches uniquely.
	devices := []ReconDevice{
		{ID: "a", CenterKHz: 500000},
		{ID: "b", CenterKHz: 500010},
	}
	observations := []Observation{
		{ID: "o-far", CenterKHz: 499995},
		{ID: "o-near", CenterKHz: 500009},
	}
	got := ReconcileObservations(devices, observations, 10)
	want := Reconciliation{
		Matched: []MatchedObservation{
			{DeviceID: "a", ObservationID: "o-far", ObservedCenter: 499995, DeviationKHz: -5},
			{DeviceID: "b", ObservationID: "o-near", ObservedCenter: 500009, DeviationKHz: -1},
		},
		Missing:    []string{},
		Unexpected: []Observation{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v; want %+v", got, want)
	}

	// With one carrier fewer, the closest claim wins outright: b takes
	// o-near (diff 1), a has nothing left and is missing.
	oneCarrier := []Observation{{ID: "o-near", CenterKHz: 500009}}
	got = ReconcileObservations(devices, oneCarrier, 10)
	want = Reconciliation{
		Matched: []MatchedObservation{
			{DeviceID: "b", ObservationID: "o-near", ObservedCenter: 500009, DeviationKHz: -1},
		},
		Missing:    []string{"a"},
		Unexpected: []Observation{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("one carrier: got %+v; want %+v", got, want)
	}
}

// The tolerance boundary is a closed comparison: exactly tolerance kHz away
// matches; one kHz more does not.
func TestReconcileObservationsToleranceBoundary(t *testing.T) {
	devices := []ReconDevice{{ID: "a", CenterKHz: 500000}}

	at := ReconcileObservations(devices, []Observation{{ID: "o", CenterKHz: 500100}}, 100)
	if len(at.Matched) != 1 || at.Matched[0].DeviationKHz != 100 {
		t.Errorf("at boundary: %+v; want one match with deviation 100", at)
	}
	over := ReconcileObservations(devices, []Observation{{ID: "o", CenterKHz: 500101}}, 100)
	if len(over.Matched) != 0 || len(over.Missing) != 1 || len(over.Unexpected) != 1 {
		t.Errorf("past boundary: %+v; want missing device and unexpected observation", over)
	}

	// Zero tolerance requires an exact carrier.
	exact := ReconcileObservations(devices, []Observation{{ID: "o", CenterKHz: 500000}}, 0)
	if len(exact.Matched) != 1 || exact.Matched[0].DeviationKHz != 0 {
		t.Errorf("zero tolerance exact: %+v; want one match deviation 0", exact)
	}
	off := ReconcileObservations(devices, []Observation{{ID: "o", CenterKHz: 500001}}, 0)
	if len(off.Matched) != 0 {
		t.Errorf("zero tolerance off by one: %+v; want no match", off)
	}
}

// Every valid record appears exactly once across the three buckets.
func TestReconcileObservationsBucketPartition(t *testing.T) {
	devices := []ReconDevice{
		{ID: "m1", CenterKHz: 500000},
		{ID: "m2", CenterKHz: 600000},
		{ID: "miss", CenterKHz: 480000},
	}
	observations := []Observation{
		{ID: "hit2", CenterKHz: 600002},
		{ID: "extra", CenterKHz: 550000},
		{ID: "hit1", CenterKHz: 499998},
	}
	got := ReconcileObservations(devices, observations, 10)
	if len(got.Matched) != 2 {
		t.Fatalf("matched = %+v; want 2", got.Matched)
	}
	if !reflect.DeepEqual(got.Missing, []string{"miss"}) {
		t.Errorf("missing = %v; want [miss]", got.Missing)
	}
	if !reflect.DeepEqual(got.Unexpected, []Observation{{ID: "extra", CenterKHz: 550000}}) {
		t.Errorf("unexpected = %+v; want [extra]", got.Unexpected)
	}
	matchedDevices := map[string]bool{}
	matchedObs := map[string]bool{}
	for _, m := range got.Matched {
		matchedDevices[m.DeviceID] = true
		matchedObs[m.ObservationID] = true
	}
	if matchedDevices["m1"] && matchedObs["hit1"] && matchedDevices["m2"] && matchedObs["hit2"] {
		return
	}
	t.Errorf("matched pairs = %+v; want m1/hit1 and m2/hit2", got.Matched)
}

// Extreme centers compare by a capped distance instead of a wrapped one: no
// spurious match arises across the int64 range.
func TestReconcileObservationsSaturatingDistance(t *testing.T) {
	devices := []ReconDevice{{ID: "lo", CenterKHz: math.MinInt64}}
	observations := []Observation{{ID: "hi", CenterKHz: math.MaxInt64}}
	got := ReconcileObservations(devices, observations, math.MaxInt64)
	if len(got.Matched) != 1 {
		t.Fatalf("tolerance MaxInt64 should match the capped distance: %+v", got)
	}
	got = ReconcileObservations(devices, observations, math.MaxInt64-1)
	if len(got.Matched) != 0 || len(got.Missing) != 1 || len(got.Unexpected) != 1 {
		t.Fatalf("tolerance MaxInt64-1 must not match the saturated pair: %+v", got)
	}
}
