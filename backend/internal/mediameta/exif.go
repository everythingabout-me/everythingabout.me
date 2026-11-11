package mediameta

import (
	"encoding/binary"
	"math"
	"strings"
)

// parseTIFF reads the EXIF facts from a TIFF structure: the payload of a JPEG APP1
// segment or an HEIF Exif item, or the start of a TIFF-based file. It is tolerant by
// construction: any directory or value that points outside data is skipped.
//
// quarterTurn reports that the EXIF orientation shows the stored pixels rotated by 90 or
// 270 degrees, for a caller that takes the pixel size from the container instead.
func parseTIFF(data []byte, info *Info) (quarterTurn bool) {
	if len(data) < 8 {
		return false
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return false
	}
	if order.Uint16(data[2:]) != 42 {
		return false
	}
	t := tiff{data: data, order: order}
	root := t.directory(int64(order.Uint32(data[4:])))
	if info.Make == nil {
		info.Make = root.text(0x010F)
	}
	if info.Model == nil {
		info.Model = root.text(0x0110)
	}
	orientation, _ := root.integer(0x0112)
	quarterTurn = orientation >= 5 && orientation <= 8

	if offset, ok := root.integer(0x8769); ok {
		exif := t.directory(int64(offset))
		// Original is when the shutter fired; Digitized is the fallback a scanner or a
		// camera without the first tag leaves. IFD0 DateTime is the last edit and is
		// deliberately not a capture time.
		for _, tags := range [][2]uint16{{0x9003, 0x9011}, {0x9004, 0x9012}} {
			if info.CapturedAt != nil {
				break
			}
			info.CapturedAt = exifTime(exif.raw(tags[0]), exif.raw(tags[1]))
		}
		if v, ok := exif.rational(0x829A); ok && v > 0 && info.ExposureSeconds == nil {
			info.ExposureSeconds = &v
		}
		if v, ok := exif.rational(0x829D); ok && v > 0 && info.FNumber == nil {
			info.FNumber = &v
		}
		if v, ok := exif.integer(0x8827); ok && v > 0 && info.ISO == nil {
			iso := int(v)
			info.ISO = &iso
		}
		if v, ok := exif.rational(0x920A); ok && v > 0 && info.FocalLength == nil {
			info.FocalLength = &v
		}
		if v, ok := exif.integer(0xA405); ok && v > 0 && info.FocalLength35mm == nil {
			equivalent := int(v)
			info.FocalLength35mm = &equivalent
		}
		if info.Lens == nil {
			info.Lens = exif.text(0xA434)
		}
		width, okW := exif.integer(0xA002)
		height, okH := exif.integer(0xA003)
		if okW && okH && info.Width == nil {
			setDimensions(info, int64(width), int64(height), quarterTurn)
		}
	}

	if offset, ok := root.integer(0x8825); ok && info.Location == nil {
		gps := t.directory(int64(offset))
		latitude, okLat := gps.degrees(0x0002, 0x0001, "S")
		longitude, okLon := gps.degrees(0x0004, 0x0003, "W")
		if okLat && okLon && validCoordinates(latitude, longitude) {
			location := &Location{Latitude: latitude, Longitude: longitude}
			if altitude, ok := gps.rational(0x0006); ok {
				if ref := gps.raw(0x0005); len(ref) > 0 && ref[0] == 1 {
					altitude = -altitude
				}
				location.Altitude = &altitude
			}
			info.Location = location
		}
	}
	return quarterTurn
}

// setDimensions records a displayed size; quarterTurn says the stored pixels are shown
// rotated by 90 or 270 degrees.
func setDimensions(info *Info, width, height int64, quarterTurn bool) {
	if width <= 0 || height <= 0 || width > 1<<30 || height > 1<<30 {
		return
	}
	w, h := int(width), int(height)
	if quarterTurn {
		w, h = h, w
	}
	info.Width, info.Height = &w, &h
}

// exifTime turns "2024:05:01 10:20:30" (and an optional "+08:00") into ISO 8601. EXIF has
// no zone of its own, so without the offset tag the result stays a zoneless wall clock.
func exifTime(value, offset []byte) *string {
	s := strings.TrimRight(string(value), "\x00 ")
	if len(s) != 19 || s[4] != ':' || s[7] != ':' || s[10] != ' ' || s[13] != ':' || s[16] != ':' {
		return nil
	}
	for i, c := range []byte(s) {
		switch i {
		case 4, 7, 10, 13, 16:
		default:
			if c < '0' || c > '9' {
				return nil
			}
		}
	}
	// Cameras with an unset clock write zeros.
	if s[:4] == "0000" || s[5:7] == "00" || s[8:10] == "00" {
		return nil
	}
	out := s[:4] + "-" + s[5:7] + "-" + s[8:10] + "T" + s[11:]
	if zone := strings.TrimRight(string(offset), "\x00 "); len(zone) == 6 && (zone[0] == '+' || zone[0] == '-') && zone[3] == ':' &&
		isDigits(zone[1:3]) && isDigits(zone[4:6]) {
		out += zone
	}
	return &out
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

type tiff struct {
	data  []byte
	order binary.ByteOrder
}

type tiffEntry struct {
	kind  uint16
	count uint32
	value []byte
}

type tiffDirectory struct {
	order   binary.ByteOrder
	entries map[uint16]tiffEntry
}

// TIFF value sizes by type: BYTE, ASCII, SHORT, LONG, RATIONAL, SBYTE, UNDEFINED, SSHORT,
// SLONG, SRATIONAL, FLOAT, DOUBLE.
var tiffSizes = [...]uint32{0, 1, 1, 2, 4, 8, 1, 1, 2, 4, 8, 4, 8}

func (t tiff) directory(offset int64) tiffDirectory {
	d := tiffDirectory{order: t.order, entries: map[uint16]tiffEntry{}}
	if offset < 8 || offset+2 > int64(len(t.data)) {
		return d
	}
	count := int64(t.order.Uint16(t.data[offset:]))
	for i := int64(0); i < count; i++ {
		at := offset + 2 + i*12
		if at+12 > int64(len(t.data)) {
			break
		}
		field := t.data[at : at+12]
		kind := t.order.Uint16(field[2:])
		n := t.order.Uint32(field[4:])
		if int(kind) >= len(tiffSizes) || kind == 0 {
			continue
		}
		size := uint64(tiffSizes[kind]) * uint64(n)
		value := field[8:12]
		if size > 4 {
			start := uint64(t.order.Uint32(field[8:]))
			if start+size > uint64(len(t.data)) {
				continue
			}
			value = t.data[start : start+size]
		} else {
			value = value[:size]
		}
		d.entries[t.order.Uint16(field)] = tiffEntry{kind: kind, count: n, value: value}
	}
	return d
}

func (d tiffDirectory) raw(tag uint16) []byte { return d.entries[tag].value }

func (d tiffDirectory) text(tag uint16) *string {
	e, ok := d.entries[tag]
	if !ok || e.kind != 2 {
		return nil
	}
	return text(e.value)
}

func (d tiffDirectory) integer(tag uint16) (uint32, bool) {
	e, ok := d.entries[tag]
	if !ok || e.count == 0 {
		return 0, false
	}
	switch e.kind {
	case 1:
		return uint32(e.value[0]), true
	case 3:
		return uint32(d.order.Uint16(e.value)), true
	case 4:
		return d.order.Uint32(e.value), true
	}
	return 0, false
}

func (d tiffDirectory) rationals(tag uint16) []float64 {
	e, ok := d.entries[tag]
	if !ok || (e.kind != 5 && e.kind != 10) {
		return nil
	}
	out := make([]float64, 0, e.count)
	for i := 0; i+8 <= len(e.value); i += 8 {
		numerator, denominator := d.order.Uint32(e.value[i:]), d.order.Uint32(e.value[i+4:])
		if denominator == 0 {
			return nil
		}
		v := float64(numerator) / float64(denominator)
		if e.kind == 10 {
			v = float64(int32(numerator)) / float64(int32(denominator))
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
		out = append(out, v)
	}
	return out
}

func (d tiffDirectory) rational(tag uint16) (float64, bool) {
	values := d.rationals(tag)
	if len(values) == 0 {
		return 0, false
	}
	return values[0], true
}

// degrees reads a GPS degrees/minutes/seconds triple and applies its hemisphere tag.
func (d tiffDirectory) degrees(tag, refTag uint16, negative string) (float64, bool) {
	parts := d.rationals(tag)
	if len(parts) != 3 {
		return 0, false
	}
	v := parts[0] + parts[1]/60 + parts[2]/3600
	ref := strings.ToUpper(strings.TrimRight(string(d.raw(refTag)), "\x00 "))
	if ref == "" {
		return 0, false
	}
	if ref == negative {
		v = -v
	}
	return v, true
}
