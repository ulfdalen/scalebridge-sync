package garmin

import (
	"encoding/binary"
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
	fit := EncodeWeightFIT(Measurement{
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
	fit := EncodeWeightFIT(Measurement{
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
	fit := EncodeWeightFIT(Measurement{
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

func TestEncodeWeightFIT_bodyScanFieldsClamped(t *testing.T) {
	// A garbage reading must not wrap into a plausible-looking number, and must
	// not land on the invalid sentinel either.
	bmr := 99999.0
	metabolicAge := 900.0
	visceral := -5.0
	fit := EncodeWeightFIT(Measurement{
		MeasuredAt:        time.Unix(1700000000, 0).UTC(),
		WeightKG:          75.0,
		BMRKcal:           &bmr,
		MetabolicAgeYears: &metabolicAge,
		VisceralFat:       &visceral,
	})

	base := firstRecord
	if got := binary.LittleEndian.Uint16(fit[base+15 : base+17]); got != 0xFFFE {
		t.Fatalf("basal_met clamp: got %04x want fffe", got)
	}
	if fit[base+17] != 0xFE {
		t.Fatalf("metabolic_age clamp: got %02x want fe", fit[base+17])
	}
	if fit[base+18] != 0 {
		t.Fatalf("visceral_fat_rating clamp: got %d want 0", fit[base+18])
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
	fit := EncodeWeightFITs(ms)

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

func TestEncodeWeightFIT_roundsAndClamps(t *testing.T) {
	weightOf := func(kg float64) uint16 {
		fit := EncodeWeightFIT(Measurement{MeasuredAt: time.Unix(1700000000, 0).UTC(), WeightKG: kg})
		off := firstRecord + 1 + 4
		return binary.LittleEndian.Uint16(fit[off : off+2])
	}

	// 77.169 × 100 = 7716.9 — truncating would write 7716 and lose 10 g.
	if got := weightOf(77.169); got != 7717 {
		t.Fatalf("rounding: got %d want 7717", got)
	}
	// 700 kg × 100 overflows uint16: clamp to 0xFFFE, since 0xFFFF means
	// "missing" and wrapping would report a plausible wrong weight.
	if got := weightOf(700); got != 0xFFFE {
		t.Fatalf("clamp: got %04x want fffe", got)
	}
}
