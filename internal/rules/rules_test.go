package rules

import (
	"math"
	"reflect"
	"testing"
)

func TestGuardKHz(t *testing.T) {
	cases := map[string]int64{
		PurposeHandheld: 125,
		PurposeBodypack: 175,
		PurposeIFB:      250,
	}
	for purpose, want := range cases {
		got, ok := GuardKHz(purpose)
		if !ok || got != want {
			t.Errorf("GuardKHz(%q) = %d, %v; want %d, true", purpose, got, ok, want)
		}
	}
	if _, ok := GuardKHz("lavalier"); ok {
		t.Errorf("GuardKHz(%q) ok = true; want false for unknown purpose", "lavalier")
	}
}

func TestProtectedIntervalEvenBandwidth(t *testing.T) {
	// Occupied [500000-100, 500000+100] = [499900, 500100], guard 125 per side.
	d := Device{ID: "mic", Purpose: PurposeHandheld, CenterKHz: 500000, BandwidthKHz: 200}
	got, ok := ProtectedInterval(d)
	want := Interval{LowKHz: 499775, HighKHz: 500225}
	if !ok || got != want {
		t.Errorf("ProtectedInterval(%+v) = %+v, %v; want %+v, true", d, got, ok, want)
	}
}

func TestProtectedIntervalOddBandwidth(t *testing.T) {
	// floor(201/2)=100, ceil(201/2)=101:
	// occupied [500000-100, 500000+101] = [499900, 500101], guard 125 per side.
	d := Device{ID: "mic", Purpose: PurposeHandheld, CenterKHz: 500000, BandwidthKHz: 201}
	got, ok := ProtectedInterval(d)
	want := Interval{LowKHz: 499775, HighKHz: 500226}
	if !ok || got != want {
		t.Errorf("ProtectedInterval(%+v) = %+v, %v; want %+v, true", d, got, ok, want)
	}
}

func TestProtectedIntervalOddBandwidthMinimum(t *testing.T) {
	// floor(25/2)=12, ceil(25/2)=13, IFB guard 250 per side.
	d := Device{ID: "mic", Purpose: PurposeIFB, CenterKHz: 600000, BandwidthKHz: 25}
	got, ok := ProtectedInterval(d)
	want := Interval{LowKHz: 599738, HighKHz: 600263}
	if !ok || got != want {
		t.Errorf("ProtectedInterval(%+v) = %+v, %v; want %+v, true", d, got, ok, want)
	}
}

func TestProtectedIntervalUnknownPurpose(t *testing.T) {
	d := Device{ID: "mic", Purpose: "lavalier", CenterKHz: 500000, BandwidthKHz: 200}
	if _, ok := ProtectedInterval(d); ok {
		t.Errorf("ProtectedInterval(%+v) ok = true; want false", d)
	}
}

// Regression: extreme center frequencies must saturate instead of wrapping
// around into an inverted, seemingly in-band interval.
func TestProtectedIntervalExtremeCenterSaturates(t *testing.T) {
	cases := []struct {
		name    string
		center  int64
		wantLow int64
		wantHi  int64
	}{
		// handheld, bw 200: half widths 100/100, guard 125 -> offsets of 225.
		{"max int64", math.MaxInt64, math.MaxInt64 - 225, math.MaxInt64},
		{"min int64", math.MinInt64, math.MinInt64, math.MinInt64 + 225},
		{"largest center that still fits", math.MaxInt64 - 225, math.MaxInt64 - 450, math.MaxInt64},
		{"smallest center that still fits", math.MinInt64 + 225, math.MinInt64, math.MinInt64 + 450},
	}
	for _, tc := range cases {
		d := Device{ID: "x", Purpose: PurposeHandheld, CenterKHz: tc.center, BandwidthKHz: 200}
		iv, ok := ProtectedInterval(d)
		if !ok {
			t.Fatalf("%s: ok = false; want true", tc.name)
		}
		if iv.LowKHz > iv.HighKHz {
			t.Errorf("%s: inverted interval [%d, %d]", tc.name, iv.LowKHz, iv.HighKHz)
		}
		if iv.LowKHz != tc.wantLow || iv.HighKHz != tc.wantHi {
			t.Errorf("%s: interval = [%d, %d]; want [%d, %d]", tc.name, iv.LowKHz, iv.HighKHz, tc.wantLow, tc.wantHi)
		}
		if InBand(iv) {
			t.Errorf("%s: InBand(%+v) = true; want false for extreme center", tc.name, iv)
		}
	}
}

func TestAdjudicateExtremeCenterRejected(t *testing.T) {
	fleet := []Device{
		{ID: "extreme", Purpose: PurposeHandheld, CenterKHz: math.MaxInt64, BandwidthKHz: 200},
		{ID: "normal", Purpose: PurposeHandheld, CenterKHz: 500000, BandwidthKHz: 200},
	}
	v := Adjudicate(fleet)
	if v.Accepted {
		t.Fatalf("Adjudicate(%+v).Accepted = true; want false for extreme center", fleet)
	}
	if len(v.OutOfBand) != 1 || v.OutOfBand[0].ID != "extreme" {
		t.Fatalf("OutOfBand = %+v; want single entry for %q", v.OutOfBand, "extreme")
	}
	iv := v.OutOfBand[0].Interval
	if iv.LowKHz > iv.HighKHz {
		t.Errorf("inverted interval [%d, %d] for extreme center", iv.LowKHz, iv.HighKHz)
	}
	// The saturated interval must not wrap around and collide with others.
	if len(v.Conflicts) != 0 {
		t.Errorf("Conflicts = %+v; want empty", v.Conflicts)
	}
}

func TestOverlaps(t *testing.T) {
	a := Interval{LowKHz: 100, HighKHz: 200}
	cases := []struct {
		name string
		b    Interval
		want bool
	}{
		{"disjoint below", Interval{LowKHz: 0, HighKHz: 99}, false},
		{"touching below", Interval{LowKHz: 0, HighKHz: 100}, true},
		{"overlap below", Interval{LowKHz: 50, HighKHz: 150}, true},
		{"contained", Interval{LowKHz: 120, HighKHz: 180}, true},
		{"identical", a, true},
		{"overlap above", Interval{LowKHz: 150, HighKHz: 250}, true},
		{"touching above", Interval{LowKHz: 200, HighKHz: 300}, true},
		{"disjoint above", Interval{LowKHz: 201, HighKHz: 300}, false},
	}
	for _, tc := range cases {
		if got := Overlaps(a, tc.b); got != tc.want {
			t.Errorf("Overlaps(%+v, %+v) = %v; want %v (%s)", a, tc.b, got, tc.want, tc.name)
		}
		if got := Overlaps(tc.b, a); got != tc.want {
			t.Errorf("Overlaps(%+v, %+v) = %v; want %v (%s, reversed)", tc.b, a, got, tc.want, tc.name)
		}
	}
}

func TestInBandEdges(t *testing.T) {
	cases := []struct {
		name string
		iv   Interval
		want bool
	}{
		{"well inside", Interval{LowKHz: 500000, HighKHz: 600000}, true},
		{"low edge exactly", Interval{LowKHz: BandLowKHz, HighKHz: 500000}, true},
		{"high edge exactly", Interval{LowKHz: 600000, HighKHz: BandHighKHz}, true},
		{"whole band", Interval{LowKHz: BandLowKHz, HighKHz: BandHighKHz}, true},
		{"one kHz below", Interval{LowKHz: BandLowKHz - 1, HighKHz: 500000}, false},
		{"one kHz above", Interval{LowKHz: 600000, HighKHz: BandHighKHz + 1}, false},
	}
	for _, tc := range cases {
		if got := InBand(tc.iv); got != tc.want {
			t.Errorf("InBand(%+v) = %v; want %v (%s)", tc.iv, got, tc.want, tc.name)
		}
	}
}

// Two handheld mics, bandwidth 200 kHz: protected interval is center ± 225.
// Centers 450 kHz apart make the protected intervals touch exactly.
func TestAdjudicateEndpointTouchingIsConflict(t *testing.T) {
	fleet := []Device{
		{ID: "a", Purpose: PurposeHandheld, CenterKHz: 500000, BandwidthKHz: 200}, // [499775, 500225]
		{ID: "b", Purpose: PurposeHandheld, CenterKHz: 500450, BandwidthKHz: 200}, // [500225, 500675]
	}
	v := Adjudicate(fleet)
	if v.Accepted {
		t.Fatalf("Adjudicate(%+v).Accepted = true; want false for touching endpoints", fleet)
	}
	want := []ConflictPair{{First: "a", Second: "b"}}
	if !reflect.DeepEqual(v.Conflicts, want) {
		t.Errorf("Conflicts = %+v; want %+v", v.Conflicts, want)
	}
}

func TestAdjudicateOneKHzApartIsAccepted(t *testing.T) {
	fleet := []Device{
		{ID: "a", Purpose: PurposeHandheld, CenterKHz: 500000, BandwidthKHz: 200}, // [499775, 500225]
		{ID: "b", Purpose: PurposeHandheld, CenterKHz: 500451, BandwidthKHz: 200}, // [500226, 500676]
	}
	v := Adjudicate(fleet)
	if !v.Accepted {
		t.Fatalf("Adjudicate(%+v).Accepted = false; want true, verdict %+v", fleet, v)
	}
	if len(v.Conflicts) != 0 || len(v.OutOfBand) != 0 {
		t.Errorf("Conflicts = %+v, OutOfBand = %+v; want both empty", v.Conflicts, v.OutOfBand)
	}
}

func TestAdjudicateMultipleConflictsSortedNormalizedDeduped(t *testing.T) {
	// All three protected intervals mutually overlap; input order is shuffled
	// and IDs are chosen so lexicographic order differs from input order.
	fleet := []Device{
		{ID: "gamma", Purpose: PurposeHandheld, CenterKHz: 500100, BandwidthKHz: 200}, // [499875, 500325]
		{ID: "alpha", Purpose: PurposeHandheld, CenterKHz: 500000, BandwidthKHz: 200}, // [499775, 500225]
		{ID: "beta", Purpose: PurposeHandheld, CenterKHz: 500050, BandwidthKHz: 200},  // [499825, 500275]
	}
	v := Adjudicate(fleet)
	if v.Accepted {
		t.Fatalf("Adjudicate(%+v).Accepted = true; want false", fleet)
	}
	want := []ConflictPair{
		{First: "alpha", Second: "beta"},
		{First: "alpha", Second: "gamma"},
		{First: "beta", Second: "gamma"},
	}
	if !reflect.DeepEqual(v.Conflicts, want) {
		t.Errorf("Conflicts = %+v; want %+v", v.Conflicts, want)
	}
	// Intervals of every device are reported sorted by ID.
	wantOrder := []string{"alpha", "beta", "gamma"}
	gotOrder := make([]string, 0, len(v.Intervals))
	for _, di := range v.Intervals {
		gotOrder = append(gotOrder, di.ID)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("Intervals order = %v; want %v", gotOrder, wantOrder)
	}
}

func TestAdjudicateOutOfBand(t *testing.T) {
	cases := []struct {
		name    string
		device  Device
		inBand  bool
		wantLow int64
		wantHi  int64
	}{
		// floor(25/2)=12, ceil(25/2)=13, handheld guard 125.
		{"low edge exact", Device{ID: "x", Purpose: PurposeHandheld, CenterKHz: 470137, BandwidthKHz: 25}, true, 470000, 470275},
		{"low edge out by one", Device{ID: "x", Purpose: PurposeHandheld, CenterKHz: 470136, BandwidthKHz: 25}, false, 469999, 470274},
		{"high edge exact", Device{ID: "x", Purpose: PurposeHandheld, CenterKHz: 693862, BandwidthKHz: 25}, true, 693725, 694000},
		{"high edge out by one", Device{ID: "x", Purpose: PurposeHandheld, CenterKHz: 693863, BandwidthKHz: 25}, false, 693726, 694001},
	}
	for _, tc := range cases {
		v := Adjudicate([]Device{tc.device})
		if v.Accepted != tc.inBand {
			t.Errorf("%s: Accepted = %v; want %v (verdict %+v)", tc.name, v.Accepted, tc.inBand, v)
		}
		if len(v.Intervals) != 1 {
			t.Fatalf("%s: got %d intervals; want 1", tc.name, len(v.Intervals))
		}
		iv := v.Intervals[0].Interval
		if iv.LowKHz != tc.wantLow || iv.HighKHz != tc.wantHi {
			t.Errorf("%s: interval = [%d, %d]; want [%d, %d]", tc.name, iv.LowKHz, iv.HighKHz, tc.wantLow, tc.wantHi)
		}
		if !tc.inBand {
			if len(v.OutOfBand) != 1 || v.OutOfBand[0].ID != tc.device.ID {
				t.Errorf("%s: OutOfBand = %+v; want single entry for %q", tc.name, v.OutOfBand, tc.device.ID)
			}
		}
	}
}

func TestAdjudicateOutOfBandDevicesStillCheckedForConflicts(t *testing.T) {
	// Both devices are out of band below 470000 and their protected
	// intervals overlap: both problems must be reported at once.
	fleet := []Device{
		{ID: "b", Purpose: PurposeHandheld, CenterKHz: 470100, BandwidthKHz: 200}, // [469875, 470325]
		{ID: "a", Purpose: PurposeHandheld, CenterKHz: 470150, BandwidthKHz: 200}, // [469925, 470375]
	}
	v := Adjudicate(fleet)
	if v.Accepted {
		t.Fatalf("Adjudicate(%+v).Accepted = true; want false", fleet)
	}
	if len(v.OutOfBand) != 2 || v.OutOfBand[0].ID != "a" || v.OutOfBand[1].ID != "b" {
		t.Errorf("OutOfBand = %+v; want [a b] sorted by ID", v.OutOfBand)
	}
	want := []ConflictPair{{First: "a", Second: "b"}}
	if !reflect.DeepEqual(v.Conflicts, want) {
		t.Errorf("Conflicts = %+v; want %+v", v.Conflicts, want)
	}
}

func TestAdjudicateAcceptedFleetIntervalsSortedByID(t *testing.T) {
	fleet := []Device{
		{ID: "mic-c", Purpose: PurposeIFB, CenterKHz: 600000, BandwidthKHz: 201},      // [599650, 600351]
		{ID: "mic-a", Purpose: PurposeHandheld, CenterKHz: 500000, BandwidthKHz: 200}, // [499775, 500225]
		{ID: "mic-b", Purpose: PurposeBodypack, CenterKHz: 550000, BandwidthKHz: 100}, // [549775, 550225]
	}
	v := Adjudicate(fleet)
	if !v.Accepted {
		t.Fatalf("Adjudicate(%+v).Accepted = false; want true, verdict %+v", fleet, v)
	}
	want := []DeviceInterval{
		{ID: "mic-a", Interval: Interval{LowKHz: 499775, HighKHz: 500225}},
		{ID: "mic-b", Interval: Interval{LowKHz: 549775, HighKHz: 550225}},
		{ID: "mic-c", Interval: Interval{LowKHz: 599650, HighKHz: 600351}},
	}
	if !reflect.DeepEqual(v.Intervals, want) {
		t.Errorf("Intervals = %+v; want %+v", v.Intervals, want)
	}
}

// A lone device far from every edge is limited by the nearer band endpoint.
func TestComputeClearanceSingleDeviceBandLimited(t *testing.T) {
	v := Adjudicate([]Device{
		{ID: "solo", Purpose: PurposeHandheld, CenterKHz: 500000, BandwidthKHz: 200}, // [499775, 500225]
	})
	if !v.Accepted {
		t.Fatalf("Accepted = false; want true, verdict %+v", v)
	}
	// band_low: 499775-470000 = 29775; band_high: 694000-500225 = 193775.
	got := ComputeClearance(v.Intervals)
	want := Clearance{
		MinimumKHz: 29775,
		Devices: []DeviceClearance{
			{ID: "solo", MinimumKHz: 29775, Limiter: LimiterBandLow},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ComputeClearance = %+v; want %+v", got, want)
	}
}

// Three devices submitted out of order: the global minimum comes from the
// gap around the middle device, not from any band edge.
func TestComputeClearanceMiddleNeighborGapDecides(t *testing.T) {
	fleet := []Device{
		{ID: "mic-c", Purpose: PurposeIFB, CenterKHz: 600000, BandwidthKHz: 201},      // [599650, 600351]
		{ID: "mic-a", Purpose: PurposeHandheld, CenterKHz: 500000, BandwidthKHz: 200}, // [499775, 500225]
		{ID: "mic-b", Purpose: PurposeHandheld, CenterKHz: 500500, BandwidthKHz: 200}, // [500275, 500725]
	}
	v := Adjudicate(fleet)
	if !v.Accepted {
		t.Fatalf("Accepted = false; want true, verdict %+v", v)
	}
	// gap a->b: 500275-500225-1 = 49; gap b->c: 599650-500725-1 = 98924.
	// c band_high: 694000-600351 = 93649 beats its other candidates.
	got := ComputeClearance(v.Intervals)
	want := Clearance{
		MinimumKHz: 49,
		Devices: []DeviceClearance{
			{ID: "mic-a", MinimumKHz: 49, Limiter: "mic-b"},
			{ID: "mic-b", MinimumKHz: 49, Limiter: "mic-a"},
			{ID: "mic-c", MinimumKHz: 93649, Limiter: LimiterBandHigh},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ComputeClearance = %+v; want %+v", got, want)
	}
}

// Protected intervals one kHz apart leave zero unoccupied ticks between
// them, so the clearance floor is exactly 0 while the fleet is accepted.
func TestComputeClearanceOneKHzApartIsZero(t *testing.T) {
	fleet := []Device{
		{ID: "a", Purpose: PurposeHandheld, CenterKHz: 500000, BandwidthKHz: 200}, // [499775, 500225]
		{ID: "b", Purpose: PurposeHandheld, CenterKHz: 500451, BandwidthKHz: 200}, // [500226, 500676]
	}
	v := Adjudicate(fleet)
	if !v.Accepted {
		t.Fatalf("Accepted = false; want true, verdict %+v", v)
	}
	got := ComputeClearance(v.Intervals)
	want := Clearance{
		MinimumKHz: 0,
		Devices: []DeviceClearance{
			{ID: "a", MinimumKHz: 0, Limiter: "b"},
			{ID: "b", MinimumKHz: 0, Limiter: "a"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ComputeClearance = %+v; want %+v", got, want)
	}
}

// Equal margins are resolved by the lexicographically smallest limiter
// identifier, independent of evaluation order.
func TestComputeClearanceTiesBreakByLimiterID(t *testing.T) {
	cases := []struct {
		name        string
		fleet       []Device
		wantID      string // device whose limiter is asserted
		wantMinimum int64
		wantLimiter string
	}{
		{
			// Centered so both band margins are 111775: "band_high" < "band_low".
			name: "band high wins tie against band low",
			fleet: []Device{
				{ID: "mic", Purpose: PurposeHandheld, CenterKHz: 582000, BandwidthKHz: 200}, // [581775, 582225]
			},
			wantID:      "mic",
			wantMinimum: 111775,
			wantLimiter: LimiterBandHigh,
		},
		{
			// zebra: band_low 100 ties the 100-tick gap to alpha; "alpha" < "band_low".
			name: "neighbor id wins tie against band low",
			fleet: []Device{
				{ID: "zebra", Purpose: PurposeHandheld, CenterKHz: 470325, BandwidthKHz: 200}, // [470100, 470550]
				{ID: "alpha", Purpose: PurposeHandheld, CenterKHz: 470876, BandwidthKHz: 200}, // [470651, 471101]
			},
			wantID:      "zebra",
			wantMinimum: 100,
			wantLimiter: "alpha",
		},
		{
			// Same 100/100 tie, but "zzz" sorts after "band_low".
			name: "band low wins tie against later neighbor id",
			fleet: []Device{
				{ID: "mic", Purpose: PurposeHandheld, CenterKHz: 470325, BandwidthKHz: 200}, // [470100, 470550]
				{ID: "zzz", Purpose: PurposeHandheld, CenterKHz: 470876, BandwidthKHz: 200}, // [470651, 471101]
			},
			wantID:      "mic",
			wantMinimum: 100,
			wantLimiter: LimiterBandLow,
		},
	}
	for _, tc := range cases {
		v := Adjudicate(tc.fleet)
		if !v.Accepted {
			t.Fatalf("%s: Accepted = false; want true, verdict %+v", tc.name, v)
		}
		got := ComputeClearance(v.Intervals)
		var entry *DeviceClearance
		for i := range got.Devices {
			if got.Devices[i].ID == tc.wantID {
				entry = &got.Devices[i]
			}
		}
		if entry == nil {
			t.Fatalf("%s: no clearance entry for %q in %+v", tc.name, tc.wantID, got)
		}
		if entry.MinimumKHz != tc.wantMinimum || entry.Limiter != tc.wantLimiter {
			t.Errorf("%s: entry = %+v; want minimum %d limiter %q", tc.name, *entry, tc.wantMinimum, tc.wantLimiter)
		}
	}
}

// Intervals derived from int64-extreme centers saturate instead of wrapping:
// margins stay well-ordered and never flip sign.
func TestComputeClearanceExtremeIntervalsSaturate(t *testing.T) {
	intervals := []DeviceInterval{
		{ID: "extreme-lo", Interval: Interval{LowKHz: math.MinInt64, HighKHz: math.MinInt64 + 225}},
		{ID: "normal", Interval: Interval{LowKHz: 499775, HighKHz: 500225}},
	}
	got := ComputeClearance(intervals)
	if len(got.Devices) != 2 {
		t.Fatalf("ComputeClearance.Devices = %+v; want 2 entries", got.Devices)
	}
	// Sorted by ID: extreme-lo first, normal second.
	lo, normal := got.Devices[0], got.Devices[1]
	if lo.ID != "extreme-lo" || lo.MinimumKHz != math.MinInt64 || lo.Limiter != LimiterBandLow {
		t.Errorf("extreme-lo entry = %+v; want saturated MinInt64 via %q", lo, LimiterBandLow)
	}
	// The gap to the saturated neighbor saturates high, so the ordinary
	// band margin still decides for the normal device.
	if normal.ID != "normal" || normal.MinimumKHz != 29775 || normal.Limiter != LimiterBandLow {
		t.Errorf("normal entry = %+v; want minimum 29775 via %q", normal, LimiterBandLow)
	}
	if got.MinimumKHz != math.MinInt64 {
		t.Errorf("MinimumKHz = %d; want %d", got.MinimumKHz, int64(math.MinInt64))
	}
}
