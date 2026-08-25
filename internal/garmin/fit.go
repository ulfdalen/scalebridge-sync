package garmin

// Encoder for Garmin's binary FIT format: https://developer.garmin.com/fit/protocol/
//
// A weight file is a 14-byte header, a file_id message, one weight_scale
// definition message followed by a data message per measurement, and a
// trailing CRC over everything before it.

import (
	"encoding/binary"
	"math"
	"time"
)

// Measurement is one weigh-in. Nil optional fields are encoded as the FIT
// "invalid" sentinel, which Garmin skips.
type Measurement struct {
	MeasuredAt   time.Time
	WeightKG     float64  // required
	BodyFatPct   *float64 // %
	MuscleKG     *float64 // kg
	BoneKG       *float64 // kg
	HydrationPct *float64 // %
	BMI          *float64 // kg/m²
}

const (
	// FIT timestamps count seconds from 1989-12-31 00:00:00 UTC.
	fitEpoch = 631065600

	// Base types; the high bit marks a multi-byte, endian-sensitive value.
	fitBaseEnum   = 0x00
	fitBaseUint8  = 0x02
	fitBaseUint16 = 0x84
	fitBaseUint32 = 0x86

	invUint16 = 0xFFFF // "missing value" sentinel

	msgFileID       = 0
	msgWeightScale  = 30
	fileTypeWeight  = 9    // file_id.type = weight_scale
	manufacturerGen = 1    // "garmin"; Connect does not enforce it
	productGeneric  = 2337 // arbitrary; Connect only groups by it
)

// EncodeWeightFIT encodes a single measurement.
func EncodeWeightFIT(m Measurement) []byte {
	return EncodeWeightFITs([]Measurement{m})
}

// EncodeWeightFITs encodes every measurement into one file: the weight_scale
// definition message is written once, then a data message each. Measurements
// must be sorted by ascending MeasuredAt. Returns nil for an empty slice.
func EncodeWeightFITs(ms []Measurement) []byte {
	if len(ms) == 0 {
		return nil
	}

	var body []byte
	body = appendFileID(body, ms[0].MeasuredAt)
	body = appendWeightScaleDef(body)
	for _, m := range ms {
		body = appendWeightScaleData(body, m)
	}

	header := make([]byte, 14)
	header[0] = 14
	header[1] = 0x20
	binary.LittleEndian.PutUint16(header[2:4], 2109)
	binary.LittleEndian.PutUint32(header[4:8], uint32(len(body)))
	copy(header[8:12], ".FIT")
	binary.LittleEndian.PutUint16(header[12:14], fitCRC(header[:12]))

	out := make([]byte, 0, len(header)+len(body)+2)
	out = append(out, header...)
	out = append(out, body...)
	return binary.LittleEndian.AppendUint16(out, fitCRC(out))
}

// ── file_id (global 0) ──────────────────────────────────

func appendFileID(body []byte, at time.Time) []byte {
	body = append(body,
		0x40,       // definition header, local 0
		0x00,       // reserved
		0x00,       // little endian
		0x00, 0x00, // global 0 (LE)
		0x04,                    // 4 fields
		0x00, 0x01, fitBaseEnum, //   type
		0x01, 0x02, fitBaseUint16, // manufacturer
		0x02, 0x02, fitBaseUint16, // product
		0x04, 0x04, fitBaseUint32, // time_created
	)
	body = append(body, 0x00)           // data header, local 0
	body = append(body, fileTypeWeight) // type = weight_scale
	body = binary.LittleEndian.AppendUint16(body, manufacturerGen)
	body = binary.LittleEndian.AppendUint16(body, productGeneric)
	body = binary.LittleEndian.AppendUint32(body, toFITTime(at))
	return body
}

// ── weight_scale (global 30) ────────────────────────────

// Written once per file; every local-1 data message that follows reuses it.
func appendWeightScaleDef(body []byte) []byte {
	body = append(body,
		0x41, // definition header, local 1
		0x00, // reserved
		0x00, // little endian
	)
	body = binary.LittleEndian.AppendUint16(body, msgWeightScale)
	return append(body,
		0x07,                     // 7 fields
		253, 0x04, fitBaseUint32, // timestamp (always present)
		0, 0x02, fitBaseUint16, //   weight
		1, 0x02, fitBaseUint16, //   percent_fat
		2, 0x02, fitBaseUint16, //   percent_hydration
		4, 0x02, fitBaseUint16, //   bone_mass
		5, 0x02, fitBaseUint16, //   muscle_mass
		13, 0x02, fitBaseUint16, //  bmi
	)
}

func appendWeightScaleData(body []byte, m Measurement) []byte {
	body = append(body, 0x01) // data header, local 1
	body = binary.LittleEndian.AppendUint32(body, toFITTime(m.MeasuredAt))
	body = binary.LittleEndian.AppendUint16(body, scaled16(m.WeightKG, 100))
	body = appendScaled16(body, m.BodyFatPct, 100)
	body = appendScaled16(body, m.HydrationPct, 100)
	body = appendScaled16(body, m.BoneKG, 100)
	body = appendScaled16(body, m.MuscleKG, 100)
	body = appendScaled16(body, m.BMI, 10)
	return body
}

func toFITTime(t time.Time) uint32 {
	return uint32(t.Unix() - fitEpoch)
}

func appendScaled16(b []byte, p *float64, scale float64) []byte {
	if p == nil {
		return binary.LittleEndian.AppendUint16(b, invUint16)
	}
	return binary.LittleEndian.AppendUint16(b, scaled16(*p, scale))
}

// scaled16 applies a FIT scale factor (100 for kg and %, 10 for BMI). It rounds
// — truncating shaves up to 10 g off a weigh-in — and clamps to 0xFFFE, since
// 0xFFFF means "missing" and wrapping would yield a plausible wrong weight.
func scaled16(v, scale float64) uint16 {
	n := math.Round(v * scale)
	if n < 0 {
		return 0
	}
	if n > invUint16-1 {
		return invUint16 - 1
	}
	return uint16(n)
}

// ── CRC-16 (FIT variant) ────────────────────────────────

// Nibble lookup table from the FIT SDK; polynomial 0xA001 reversed.
var fitCRCTable = [16]uint16{
	0x0000, 0xCC01, 0xD801, 0x1400,
	0xF001, 0x3C00, 0x2800, 0xE401,
	0xA001, 0x6C00, 0x7800, 0xB401,
	0x5000, 0x9C01, 0x8801, 0x4400,
}

func fitCRC(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		tmp := fitCRCTable[crc&0xF]
		crc = (crc >> 4) & 0x0FFF
		crc = crc ^ tmp ^ fitCRCTable[b&0xF]

		tmp = fitCRCTable[crc&0xF]
		crc = (crc >> 4) & 0x0FFF
		crc = crc ^ tmp ^ fitCRCTable[(b>>4)&0xF]
	}
	return crc
}
