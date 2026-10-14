package garmin

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func TestEncodeWeightFIT_structure(t *testing.T) {
	at := time.Date(2026, 4, 22, 13, 26, 0, 0, time.UTC)
	fatVal := 18.2
	muscleVal := 60.2
	boneVal := 3.17
	hydrationVal := 45.8
	bmiVal := 24.8
	fit := mustEncode(t, Measurement{
		MeasuredAt:   at,
		WeightKG:     80.5,
		BodyFatPct:   &fatVal,
		MuscleKG:     &muscleVal,
		BoneKG:       &boneVal,
		HydrationPct: &hydrationVal,
		BMI:          &bmiVal,
	})

	if fit[0] != 14 {
		t.Fatalf("header size: got %d want 14", fit[0])
	}
	if string(fit[8:12]) != ".FIT" {
		t.Fatalf("signature: got %q want .FIT", string(fit[8:12]))
	}

	// The header's data size counts the body only: total minus the 14-byte
	// header and the trailing 2-byte CRC.
	dataSize := binary.LittleEndian.Uint32(fit[4:8])
	if int(dataSize) != len(fit)-14-2 {
		t.Fatalf("data size mismatch: header says %d, actual body %d", dataSize, len(fit)-14-2)
	}

	trailingCRC := binary.LittleEndian.Uint16(fit[len(fit)-2:])
	if got := fitCRC(fit[:len(fit)-2]); got != trailingCRC {
		t.Fatalf("trailing CRC mismatch: got %04x computed %04x", trailingCRC, got)
	}

	hdrCRC := binary.LittleEndian.Uint16(fit[12:14])
	if got := fitCRC(fit[:12]); got != hdrCRC {
		t.Fatalf("header CRC mismatch: got %04x computed %04x", hdrCRC, got)
	}
}

func TestEncodeWeightFIT_optionalFieldsInvalid(t *testing.T) {
	// Nil body-composition fields must encode as the invalid sentinel — 0xFFFF
	// for uint16, 0xFF for uint8 — which Garmin's parser skips.
	fit := mustEncode(t, Measurement{
		MeasuredAt: time.Unix(1700000000, 0).UTC(),
		WeightKG:   75.0,
	})

	base := firstRecord
	if fit[base] != 0x01 {
		t.Fatalf("data header: got %02x want 01", fit[base])
	}
	weight := binary.LittleEndian.Uint16(fit[base+1+4 : base+1+4+2])
	if weight != 7500 {
		t.Fatalf("weight encoded: got %d want 7500 (75.0kg × 100)", weight)
	}
	// percent_fat, percent_hydration, bone_mass, muscle_mass, basal_met.
	for i := 0; i < 5; i++ {
		off := base + 7 + i*2
		got := binary.LittleEndian.Uint16(fit[off : off+2])
		if got != 0xFFFF {
			t.Fatalf("optional uint16 field %d: got %04x want ffff (invalid)", i, got)
		}
	}
	if fit[base+17] != 0xFF {
		t.Fatalf("metabolic_age: got %02x want ff (invalid)", fit[base+17])
	}
	if fit[base+18] != 0xFF {
		t.Fatalf("visceral_fat_rating: got %02x want ff (invalid)", fit[base+18])
	}
	if got := binary.LittleEndian.Uint16(fit[base+19 : base+21]); got != 0xFFFF {
		t.Fatalf("bmi: got %04x want ffff (invalid)", got)
	}
}

func TestEncodeWeightFIT_bodyScanFields(t *testing.T) {
	bmr := 1685.5
	metabolicAge := 34.0
	visceral := 8.0
	fit := mustEncode(t, Measurement{
		MeasuredAt:        time.Unix(1700000000, 0).UTC(),
		WeightKG:          75.0,
		BMRKcal:           &bmr,
		MetabolicAgeYears: &metabolicAge,
		VisceralFat:       &visceral,
	})

	base := firstRecord
	// basal_met is scale 4: kcal/day × 4, rounded.
	if got := binary.LittleEndian.Uint16(fit[base+15 : base+17]); got != 6742 {
		t.Fatalf("basal_met: got %d want 6742 (1685.5 × 4)", got)
	}
	if fit[base+17] != 34 {
		t.Fatalf("metabolic_age: got %d want 34", fit[base+17])
	}
	if fit[base+18] != 8 {
		t.Fatalf("visceral_fat_rating: got %d want 8", fit[base+18])
	}
}

func TestEncodeWeightFIT_unrepresentableValuesOmitted(t *testing.T) {
	// A value a field can't hold becomes the invalid sentinel: never clamped to
	// the field's maximum, never zeroed, never wrapped. Each of those would
	// land in Garmin Connect as a plausible reading nobody measured.
	ptr := func(v float64) *float64 { return &v }
	fit := mustEncode(t, Measurement{
		MeasuredAt:        time.Unix(1700000000, 0).UTC(),
		WeightKG:          75.0,
		BodyFatPct:        ptr(math.NaN()),
		HydrationPct:      ptr(655.35), // × 100 = 0xFFFF, the sentinel itself
		BoneKG:            ptr(0.004),  // rounds to 0
		MuscleKG:          ptr(math.Inf(1)),
		BMRKcal:           ptr(99999), // × 4 overflows uint16
		MetabolicAgeYears: ptr(900),   // overflows uint8
		VisceralFat:       ptr(-5),
		BMI:               ptr(-0.01),
	})

	base := firstRecord
	for i, name := range []string{"percent_fat", "percent_hydration", "bone_mass", "muscle_mass", "basal_met"} {
		off := base + 7 + i*2
		if got := binary.LittleEndian.Uint16(fit[off : off+2]); got != 0xFFFF {
			t.Errorf("%s: got %04x want ffff (invalid)", name, got)
		}
	}
	if fit[base+17] != 0xFF {
		t.Errorf("metabolic_age: got %02x want ff (invalid)", fit[base+17])
	}
	if fit[base+18] != 0xFF {
		t.Errorf("visceral_fat_rating: got %02x want ff (invalid)", fit[base+18])
	}
	if got := binary.LittleEndian.Uint16(fit[base+19 : base+21]); got != 0xFFFF {
		t.Errorf("bmi: got %04x want ffff (invalid)", got)
	}
}

func TestEncodeWeightFIT_largestValuesKept(t *testing.T) {
	// One short of the sentinel is still a value.
	ptr := func(v float64) *float64 { return &v }
	fit := mustEncode(t, Measurement{
		MeasuredAt:        time.Unix(1700000000, 0).UTC(),
		WeightKG:          75.0,
		BodyFatPct:        ptr(655.34),
		MetabolicAgeYears: ptr(254),
		VisceralFat:       ptr(1),
	})

	base := firstRecord
	if got := binary.LittleEndian.Uint16(fit[base+7 : base+9]); got != 0xFFFE {
		t.Errorf("percent_fat: got %04x want fffe", got)
	}
	if fit[base+17] != 0xFE {
		t.Errorf("metabolic_age: got %02x want fe", fit[base+17])
	}
	if fit[base+18] != 1 {
		t.Errorf("visceral_fat_rating: got %d want 1", fit[base+18])
	}
}

func TestEncodeWeightFIT_definition(t *testing.T) {
	// The data messages are positional: if the definition's field list ever
	// drifts, every value lands in the wrong field.
	fit := mustEncode(t, Measurement{MeasuredAt: time.Unix(1700000000, 0).UTC(), WeightKG: 75})

	def := fit[14+18+10 : firstRecord]
	if def[0] != 0x41 {
		t.Fatalf("definition header: got %02x want 41 (definition, local 1)", def[0])
	}
	if def[2] != 0x00 {
		t.Fatalf("architecture: got %02x want 00 (little endian)", def[2])
	}
	if got := binary.LittleEndian.Uint16(def[3:5]); got != 30 {
		t.Fatalf("global message: got %d want 30 (weight_scale)", got)
	}
	if def[5] != 10 {
		t.Fatalf("field count: got %d want 10", def[5])
	}
	want := [][3]byte{
		{253, 4, 0x86}, // timestamp
		{0, 2, 0x84},   // weight
		{1, 2, 0x84},   // percent_fat
		{2, 2, 0x84},   // percent_hydration
		{4, 2, 0x84},   // bone_mass
		{5, 2, 0x84},   // muscle_mass
		{7, 2, 0x84},   // basal_met
		{10, 1, 0x02},  // metabolic_age
		{11, 1, 0x02},  // visceral_fat_rating
		{13, 2, 0x84},  // bmi
	}
	for i, w := range want {
		var got [3]byte
		copy(got[:], def[6+i*3:9+i*3])
		if got != w {
			t.Errorf("field %d: got (%d,%d,%#02x) want (%d,%d,%#02x)", i, got[0], got[1], got[2], w[0], w[1], w[2])
		}
	}
}

// Where the first weight_scale data message starts, and how long each one is:
// 14 header + 18 file_id def + 10 file_id data + 36 weight_scale def, then
// 1 header + 4 timestamp + 7 × uint16 + 2 × uint8 per record.
const (
	firstRecord = 14 + 18 + 10 + 36
	recordLen   = 1 + 4 + 7*2 + 2
)

func TestEncodeWeightFITs_multiRecord(t *testing.T) {
	at := time.Date(2026, 4, 22, 13, 26, 0, 0, time.UTC)
	fatVal := 18.2
	ms := []Measurement{
		{MeasuredAt: at, WeightKG: 80.5, BodyFatPct: &fatVal},
		{MeasuredAt: at.Add(24 * time.Hour), WeightKG: 80.1}, // no optionals
		{MeasuredAt: at.Add(48 * time.Hour), WeightKG: 79.9, BodyFatPct: &fatVal},
	}
	fit, err := EncodeWeightFITs(ms)
	if err != nil {
		t.Fatalf("EncodeWeightFITs: %v", err)
	}

	// The exact length proves the definition message appears once: a repeat
	// would add 36 bytes.
	if want := firstRecord + len(ms)*recordLen + 2; len(fit) != want {
		t.Fatalf("length: got %d want %d (definition message must be written once)", len(fit), want)
	}
	if def := fit[14+18+10]; def != 0x41 {
		t.Fatalf("weight_scale definition header: got %02x want 41", def)
	}

	dataSize := binary.LittleEndian.Uint32(fit[4:8])
	if int(dataSize) != len(fit)-14-2 {
		t.Fatalf("data size mismatch: header says %d, actual body %d", dataSize, len(fit)-14-2)
	}
	trailingCRC := binary.LittleEndian.Uint16(fit[len(fit)-2:])
	if got := fitCRC(fit[:len(fit)-2]); got != trailingCRC {
		t.Fatalf("trailing CRC mismatch: got %04x computed %04x", trailingCRC, got)
	}

	var prev uint32
	for i, m := range ms {
		off := firstRecord + i*recordLen
		if fit[off] != 0x01 {
			t.Fatalf("record %d data header: got %02x want 01", i, fit[off])
		}
		ts := binary.LittleEndian.Uint32(fit[off+1 : off+5])
		if want := toFITTime(m.MeasuredAt); ts != want {
			t.Fatalf("record %d timestamp: got %d want %d", i, ts, want)
		}
		if i > 0 && ts <= prev {
			t.Fatalf("record %d timestamp %d not ascending (previous %d)", i, ts, prev)
		}
		prev = ts
	}

	// The second record carries no body composition — every optional is invalid.
	second := firstRecord + recordLen
	for i := 0; i < 5; i++ { // percent_fat … basal_met
		off := second + 7 + i*2
		if got := binary.LittleEndian.Uint16(fit[off : off+2]); got != 0xFFFF {
			t.Fatalf("record 1 optional uint16 field %d: got %04x want ffff (invalid)", i, got)
		}
	}
	if fit[second+17] != 0xFF || fit[second+18] != 0xFF {
		t.Fatalf("record 1 uint8 optionals: got %02x,%02x want ff,ff (invalid)", fit[second+17], fit[second+18])
	}
	if got := binary.LittleEndian.Uint16(fit[second+19 : second+21]); got != 0xFFFF {
		t.Fatalf("record 1 bmi: got %04x want ffff (invalid)", got)
	}
}

func TestEncodeWeightFIT_weightRounded(t *testing.T) {
	weightOf := func(kg float64) uint16 {
		fit := mustEncode(t, Measurement{MeasuredAt: time.Unix(1700000000, 0).UTC(), WeightKG: kg})
		off := firstRecord + 1 + 4
		return binary.LittleEndian.Uint16(fit[off : off+2])
	}

	// 77.169 × 100 = 7716.9 — truncating would write 7716 and lose 10 g.
	if got := weightOf(77.169); got != 7717 {
		t.Fatalf("rounding: got %d want 7717", got)
	}
	// 0xFFFD is the largest weight: 0xFFFE means "calculating", 0xFFFF missing.
	if got := weightOf(655.33); got != 0xFFFD {
		t.Fatalf("largest weight: got %04x want fffd", got)
	}
}

func TestEncodeWeightFIT_unencodableWeightIsAnError(t *testing.T) {
	// Weight is the record: one that can't be encoded must not be clamped to
	// 655.34 kg ("calculating") or zeroed.
	for _, kg := range []float64{0, -1, 0.004, 655.34, 700, math.NaN(), math.Inf(1)} {
		fit, err := EncodeWeightFIT(Measurement{MeasuredAt: time.Unix(1700000000, 0).UTC(), WeightKG: kg})
		if err == nil {
			t.Errorf("weight %v: encoded %d bytes, want an error", kg, len(fit))
		}
	}
}

func TestEncodeWeightFIT_unencodableTimestampIsAnError(t *testing.T) {
	// Below 0x10000000 a FIT date_time means "seconds since power-on", and
	// 0xFFFFFFFF is invalid; a zero or pre-1998 time would wrap into 2042 or 2106.
	for _, at := range []time.Time{
		{},
		time.Unix(0, 0),
		time.Unix(fitEpoch-1, 0),
		time.Unix(fitEpoch+0x10000000-1, 0), // 1998-07-03 21:24:15 UTC
		time.Unix(fitEpoch+0xFFFFFFFF, 0),
	} {
		if fit, err := EncodeWeightFIT(Measurement{MeasuredAt: at, WeightKG: 70}); err == nil {
			t.Errorf("timestamp %v: encoded %d bytes, want an error", at.UTC(), len(fit))
		}
	}
	for _, at := range []time.Time{
		time.Unix(fitEpoch+0x10000000, 0), // 1998-07-03 21:24:16 UTC
		time.Unix(fitEpoch+0xFFFFFFFE, 0),
	} {
		if _, err := EncodeWeightFIT(Measurement{MeasuredAt: at, WeightKG: 70}); err != nil {
			t.Errorf("timestamp %v: %v, want it encoded", at.UTC(), err)
		}
	}
}

func TestEncodeWeightFITs_oneBadRecordFailsTheBatch(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	fit, err := EncodeWeightFITs([]Measurement{
		{MeasuredAt: at, WeightKG: 80},
		{MeasuredAt: at.Add(time.Hour), WeightKG: 0},
	})
	if err == nil || fit != nil {
		t.Fatalf("got %d bytes, err %v; want nil and an error", len(fit), err)
	}
}

func mustEncode(t *testing.T, m Measurement) []byte {
	t.Helper()
	fit, err := EncodeWeightFIT(m)
	if err != nil {
		t.Fatalf("EncodeWeightFIT: %v", err)
	}
	return fit
}
