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
	// Nil body-composition fields must encode as the 0xFFFF invalid sentinel,
	// which Garmin's parser skips.
	fit := EncodeWeightFIT(Measurement{
		MeasuredAt: time.Unix(1700000000, 0).UTC(),
		WeightKG:   75.0,
	})

	base := 14 + 18 + 10 + 27 // start of the data message; see firstRecord below
	if fit[base] != 0x01 {
		t.Fatalf("data header: got %02x want 01", fit[base])
	}
	weight := binary.LittleEndian.Uint16(fit[base+1+4 : base+1+4+2])
	if weight != 7500 {
		t.Fatalf("weight encoded: got %d want 7500 (75.0kg × 100)", weight)
	}
	for i := 0; i < 5; i++ {
		off := base + 1 + 4 + 2 + i*2
		got := binary.LittleEndian.Uint16(fit[off : off+2])
		if got != 0xFFFF {
			t.Fatalf("optional field %d: got %04x want ffff (invalid)", i, got)
		}
	}
}

// Where the first weight_scale data message starts, and how long each one is:
// 14 header + 18 file_id def + 10 file_id data + 27 weight_scale def, then
// 1 header + 4 timestamp + 6 × 2 fields per record.
const (
	firstRecord = 14 + 18 + 10 + 27
	recordLen   = 1 + 4 + 6*2
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
	// would add 27 bytes.
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

	// The second record carries no body composition — all five are invalid.
	for i := 0; i < 5; i++ {
		off := firstRecord + recordLen + 1 + 4 + 2 + i*2
		if got := binary.LittleEndian.Uint16(fit[off : off+2]); got != 0xFFFF {
			t.Fatalf("record 1 optional field %d: got %04x want ffff (invalid)", i, got)
		}
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
