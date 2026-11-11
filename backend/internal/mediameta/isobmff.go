package mediameta

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// box is one ISO base media box located in the file, not yet read.
type box struct {
	kind      string
	body, end int64
}

// readBox reads the header at offset inside a container that ends at limit.
func readBox(r *window, offset, limit int64) (box, error) {
	if offset < 0 || offset > limit-8 {
		return box{}, errMalformed
	}
	head, err := r.bytes(offset, 8)
	if err != nil {
		return box{}, err
	}
	size, body := int64(be32(head)), offset+8
	switch size {
	case 0: // extends to the end of its container
		size = limit - offset
	case 1: // 64-bit size follows the type
		if offset > limit-16 {
			return box{}, errMalformed
		}
		large, err := r.bytes(offset+8, 8)
		if err != nil {
			return box{}, err
		}
		if be64(large) > uint64(limit-offset) {
			return box{}, errMalformed
		}
		size, body = int64(be64(large)), offset+16
	}
	if size < body-offset || size > limit-offset {
		return box{}, errMalformed
	}
	return box{kind: string(head[4:8]), body: body, end: offset + size}, nil
}

// region copies a small box payload out of the window. A payload larger than any real
// metadata region is reported as absent rather than read.
func region(r *window, b box) ([]byte, error) {
	if b.end-b.body > maxRegion {
		return nil, nil
	}
	return r.bytes(b.body, int(b.end-b.body))
}

type memBox struct {
	kind    string
	payload []byte
}

// memBoxes splits an in-memory payload into its child boxes and stops at the first one
// that does not fit, which is also how a QuickTime user-data terminator ends the list.
func memBoxes(data []byte) []memBox {
	var out []memBox
	for len(data) >= 8 && len(out) < 4096 {
		size, header := uint64(be32(data)), uint64(8)
		switch size {
		case 0:
			size = uint64(len(data))
		case 1:
			if len(data) < 16 {
				return out
			}
			size, header = be64(data[8:]), 16
		}
		if size < header || size > uint64(len(data)) {
			return out
		}
		out = append(out, memBox{kind: string(data[4:8]), payload: data[header:size]})
		data = data[size:]
	}
	return out
}

// parseISOBMFF walks the top-level boxes of an HEIF image or an MP4/QuickTime movie. The
// media data box is skipped by its header alone, which is why a movie whose header sits
// after gigabytes of samples still costs a few reads.
func parseISOBMFF(r *window, info *Info) error {
	offset := int64(0)
	// Ordinary files have under ten top-level boxes. A fragmented movie has one pair per
	// fragment, all after the header this needs, so the bound ends that walk early.
	for n := 0; n < 64 && offset < r.size; n++ {
		b, err := readBox(r, offset, r.size)
		if err != nil {
			return err
		}
		switch b.kind {
		case "meta":
			data, err := region(r, b)
			if err != nil {
				return err
			}
			if err := parseHEIFMeta(r, data, info); err != nil {
				return err
			}
		case "moov":
			if err := parseMovie(r, b, info); err != nil {
				return err
			}
		}
		offset = b.end
	}
	return nil
}

// parseHEIFMeta reads an HEIF item directory: where the Exif item lives, and the size
// and rotation properties of the image.
func parseHEIFMeta(r *window, data []byte, info *Info) error {
	if len(data) < 4 {
		return nil
	}
	var (
		exifItem      uint32
		hasExif       bool
		locations     []byte
		width, height int64
		quarterTurn   bool
	)
	for _, child := range memBoxes(data[4:]) {
		switch child.kind {
		case "iinf":
			exifItem, hasExif = heifExifItem(child.payload)
		case "iloc":
			locations = child.payload
		case "iprp":
			for _, container := range memBoxes(child.payload) {
				if container.kind != "ipco" {
					continue
				}
				for _, property := range memBoxes(container.payload) {
					switch {
					// The primary image is the largest one described; the others are
					// thumbnails and the tiles it is assembled from.
					case property.kind == "ispe" && len(property.payload) >= 12:
						w, h := int64(be32(property.payload[4:])), int64(be32(property.payload[8:]))
						if w*h > width*height {
							width, height = w, h
						}
					case property.kind == "irot" && len(property.payload) >= 1:
						quarterTurn = quarterTurn || property.payload[0]&1 == 1
					}
				}
			}
		}
	}
	if hasExif {
		if offset, length, ok := heifItemExtent(locations, exifItem); ok {
			if length > maxRegion {
				length = maxRegion
			}
			payload, err := r.bytes(offset, int(length))
			var stop stopError
			if err != nil && !errors.As(err, &stop) {
				return err
			}
			// An Exif item that points outside the file is skipped; the size and
			// rotation properties already read are still worth reporting.
			if err == nil && len(payload) > 4 {
				// The item starts with the offset of the TIFF header within it.
				if skip := int64(be32(payload)); skip <= int64(len(payload))-4 {
					parseTIFF(payload[4+skip:], info)
				}
			}
		}
	}
	// HEIF states size and rotation as item properties, and its readers ignore the EXIF
	// orientation in favor of them, so they win here too.
	if width > 0 && height > 0 {
		setDimensions(info, width, height, quarterTurn)
	}
	return nil
}

// heifExifItem finds the item whose type is "Exif" in an item information box.
func heifExifItem(data []byte) (uint32, bool) {
	if len(data) < 8 {
		return 0, false
	}
	entries := data[6:]
	if data[0] != 0 {
		entries = data[8:]
	}
	for _, entry := range memBoxes(entries) {
		if entry.kind != "infe" || len(entry.payload) < 4 {
			continue
		}
		fields := entry.payload[4:]
		switch {
		case entry.payload[0] == 2 && len(fields) >= 8 && string(fields[4:8]) == "Exif":
			return uint32(be16(fields)), true
		case entry.payload[0] == 3 && len(fields) >= 10 && string(fields[6:10]) == "Exif":
			return be32(fields), true
		}
	}
	return 0, false
}

// cursor reads the variable-width integers of an item location box.
type cursor struct {
	data []byte
	bad  bool
}

func (c *cursor) uint(width int) uint64 {
	if c.bad || width < 0 || width > 8 || len(c.data) < width {
		c.bad = true
		return 0
	}
	var v uint64
	for _, b := range c.data[:width] {
		v = v<<8 | uint64(b)
	}
	c.data = c.data[width:]
	return v
}

// heifItemExtent returns where an item's bytes are in the file. Only an item stored as
// one extent addressed by file offset is resolved: that is how every camera writes Exif,
// and the other forms would need boxes this parser does not otherwise read.
func heifItemExtent(data []byte, item uint32) (offset, length int64, ok bool) {
	if len(data) < 6 {
		return 0, 0, false
	}
	version := data[0]
	offsetSize, lengthSize := int(data[4]>>4), int(data[4]&0xF)
	baseSize, indexSize := int(data[5]>>4), int(data[5]&0xF)
	if version == 0 {
		indexSize = 0
	}
	c := &cursor{data: data[6:]}
	idSize := 2
	if version >= 2 {
		idSize = 4
	}
	count := c.uint(idSize)
	for i := uint64(0); i < count && !c.bad; i++ {
		id := c.uint(idSize)
		method := uint64(0)
		if version >= 1 {
			method = c.uint(2) & 0xF
		}
		reference := c.uint(2)
		base := c.uint(baseSize)
		extents := c.uint(2)
		var first, size uint64
		for e := uint64(0); e < extents && !c.bad; e++ {
			c.uint(indexSize)
			extentOffset, extentLength := c.uint(offsetSize), c.uint(lengthSize)
			if e == 0 {
				first, size = extentOffset, extentLength
			}
		}
		if c.bad || id != uint64(item) {
			continue
		}
		if method != 0 || reference != 0 || extents != 1 || size == 0 || base > math.MaxInt64/2 || first > math.MaxInt64/2 || size > math.MaxInt64/2 {
			return 0, 0, false
		}
		return int64(base + first), int64(size), true
	}
	return 0, 0, false
}

// movieFacts collects what a movie states in several places, so the most specific
// statement can be chosen once everything has been read.
type movieFacts struct {
	keyedTime, userTime, headerTime *string
	keyedPlace, userPlace           *Location
	keyedMake, keyedModel           *string
	userMake, userModel             *string
	width, height                   int64
	quarterTurn                     bool
}

// parseMovie reads the movie header, each track header and the user data. Sample tables,
// which make up nearly all of a movie box, are skipped by their headers.
func parseMovie(r *window, moov box, info *Info) error {
	var facts movieFacts
	err := walkMovie(r, moov, info, &facts)
	// Whatever was established before a stop is still what the file states.
	for _, candidate := range []*string{facts.keyedTime, facts.userTime, facts.headerTime} {
		if info.CapturedAt == nil {
			info.CapturedAt = candidate
		}
	}
	for _, candidate := range []*Location{facts.keyedPlace, facts.userPlace} {
		if info.Location == nil {
			info.Location = candidate
		}
	}
	for _, candidate := range []*string{facts.keyedMake, facts.userMake} {
		if info.Make == nil {
			info.Make = candidate
		}
	}
	for _, candidate := range []*string{facts.keyedModel, facts.userModel} {
		if info.Model == nil {
			info.Model = candidate
		}
	}
	if facts.width > 0 && facts.height > 0 {
		setDimensions(info, facts.width, facts.height, facts.quarterTurn)
	}
	return err
}

func walkMovie(r *window, moov box, info *Info, facts *movieFacts) error {
	offset := moov.body
	for n := 0; n < 64 && offset < moov.end; n++ {
		b, err := readBox(r, offset, moov.end)
		if err != nil {
			return err
		}
		switch b.kind {
		case "mvhd":
			if err := parseMovieHeader(r, b, info, facts); err != nil {
				return err
			}
		case "trak":
			if err := parseTrack(r, b, facts); err != nil {
				return err
			}
		case "udta":
			data, err := region(r, b)
			if err != nil {
				return err
			}
			parseUserData(data, facts)
		case "meta":
			data, err := region(r, b)
			if err != nil {
				return err
			}
			parseKeyedMetadata(data, facts)
		}
		offset = b.end
	}
	return nil
}

// Movie times count seconds from 1904-01-01 UTC.
const movieEpochOffset = 2082844800

func parseMovieHeader(r *window, b box, info *Info, facts *movieFacts) error {
	n := b.end - b.body
	if n > 32 {
		n = 32
	}
	data, err := r.bytes(b.body, int(n))
	if err != nil {
		return err
	}
	var created, duration uint64
	var timescale uint32
	switch {
	case len(data) >= 20 && data[0] == 0:
		created, timescale, duration = uint64(be32(data[4:])), be32(data[12:]), uint64(be32(data[16:]))
		if duration == math.MaxUint32 {
			duration = 0
		}
	case len(data) >= 32 && data[0] == 1:
		created, timescale, duration = be64(data[4:]), be32(data[20:]), be64(data[24:])
		if duration == math.MaxUint64 {
			duration = 0
		}
	default:
		return nil
	}
	if timescale > 0 && duration > 0 {
		seconds := math.Round(float64(duration)/float64(timescale)*1000) / 1000
		info.DurationSeconds = &seconds
	}
	// Encoders that do not know the time write zero, and some write Unix seconds into
	// this 1904-based field; both land decades before any digital video and are dropped.
	if unix := int64(created) - movieEpochOffset; created < math.MaxInt64 && unix >= 631152000 && unix < 4102444800 {
		formatted := time.Unix(unix, 0).UTC().Format("2006-01-02T15:04:05Z")
		facts.headerTime = &formatted
	}
	return nil
}

// parseTrack reads the track header for the presentation size. The visual track is the
// largest one; audio and metadata tracks state zero.
func parseTrack(r *window, trak box, facts *movieFacts) error {
	offset := trak.body
	for n := 0; n < 16 && offset < trak.end; n++ {
		b, err := readBox(r, offset, trak.end)
		if err != nil {
			return err
		}
		offset = b.end
		if b.kind != "tkhd" {
			continue
		}
		length := b.end - b.body
		if length > 96 {
			length = 96
		}
		data, err := r.bytes(b.body, int(length))
		if err != nil {
			return err
		}
		matrix := 40
		if len(data) > 0 && data[0] == 1 {
			matrix = 52
		}
		if len(data) < matrix+44 {
			return nil
		}
		width, height := int64(be32(data[matrix+36:])>>16), int64(be32(data[matrix+40:])>>16)
		if width*height > facts.width*facts.height {
			facts.width, facts.height = width, height
			// A matrix with no scale on its diagonal is a quarter turn: phones record
			// portrait video as landscape samples plus this rotation.
			a, bb, c, d := be32(data[matrix:]), be32(data[matrix+4:]), be32(data[matrix+12:]), be32(data[matrix+16:])
			facts.quarterTurn = a == 0 && d == 0 && bb != 0 && c != 0
		}
		return nil
	}
	return nil
}

// parseUserData reads the classic QuickTime annotations Android and older cameras write.
func parseUserData(data []byte, facts *movieFacts) {
	for _, item := range memBoxes(data) {
		switch item.kind {
		case "meta":
			parseKeyedMetadata(item.payload, facts)
		case "\xa9xyz":
			if facts.userPlace == nil {
				facts.userPlace = iso6709(annotation(item.payload))
			}
		case "\xa9day":
			if facts.userTime == nil {
				facts.userTime = isoTime(annotation(item.payload))
			}
		case "\xa9mak":
			facts.userMake = text([]byte(annotation(item.payload)))
		case "\xa9mod":
			facts.userModel = text([]byte(annotation(item.payload)))
		}
	}
}

// annotation returns the text of a user-data item, which is either a length and language
// followed by the string, or an iTunes-style "data" box.
func annotation(payload []byte) string {
	if len(payload) >= 16 && string(payload[4:8]) == "data" {
		return string(payload[16:])
	}
	if len(payload) < 4 {
		return ""
	}
	length := int(be16(payload))
	if length > len(payload)-4 {
		length = len(payload) - 4
	}
	return string(payload[4 : 4+length])
}

// parseKeyedMetadata reads the key/value metadata Apple devices and newer Android
// recorders write: a table of key names and a list of values addressed by key index.
func parseKeyedMetadata(data []byte, facts *movieFacts) {
	// QuickTime writes this box as a plain container, ISO files as a full box with four
	// leading version bytes. The first child's type tells them apart.
	if len(data) >= 8 && !knownMetaChild(string(data[4:8])) {
		data = data[4:]
	}
	var keys []string
	var values []memBox
	for _, child := range memBoxes(data) {
		switch child.kind {
		case "keys":
			if len(child.payload) < 8 {
				continue
			}
			for _, key := range memBoxes(child.payload[8:]) {
				keys = append(keys, string(key.payload))
			}
		case "ilst":
			values = memBoxes(child.payload)
		}
	}
	for _, value := range values {
		index := int(be32([]byte(value.kind)))
		if index < 1 || index > len(keys) {
			continue
		}
		var content string
		for _, part := range memBoxes(value.payload) {
			// Type 1 is UTF-8, the only encoding the keys below use.
			if part.kind == "data" && len(part.payload) >= 8 && be32(part.payload)&0xFFFFFF == 1 {
				content = string(part.payload[8:])
			}
		}
		if content == "" {
			continue
		}
		switch keys[index-1] {
		case "com.apple.quicktime.creationdate":
			facts.keyedTime = isoTime(content)
		case "com.apple.quicktime.location.ISO6709":
			facts.keyedPlace = iso6709(content)
		case "com.apple.quicktime.make", "com.android.manufacturer":
			facts.keyedMake = text([]byte(content))
		case "com.apple.quicktime.model", "com.android.model":
			facts.keyedModel = text([]byte(content))
		}
	}
}

func knownMetaChild(kind string) bool {
	return kind == "hdlr" || kind == "keys" || kind == "ilst" || kind == "free"
}

// isoTime normalizes a full ISO 8601 date-time. The recorded offset is kept as written,
// including +00:00: rewriting that to "Z" would turn a stated wall clock into a bare
// instant. A year or a date alone is not a capture time and is dropped.
func isoTime(value string) *string {
	value = strings.Trim(value, "\x00 \t\r\n")
	for _, layout := range []string{"2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05.999999999Z0700", "2006-01-02T15:04:05.999999999Z07:00", "2006-01-02 15:04:05Z0700"} {
		parsed, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		format := "2006-01-02T15:04:05-07:00"
		if strings.HasSuffix(value, "Z") {
			format = "2006-01-02T15:04:05Z"
		}
		formatted := parsed.Format(format)
		return &formatted
	}
	return nil
}

var iso6709Pattern = regexp.MustCompile(`^([+-]\d+(?:\.\d+)?)([+-]\d+(?:\.\d+)?)([+-]\d+(?:\.\d+)?)?`)

// iso6709 parses "+31.2304+121.4737+004.000/". Only decimal degrees are accepted; the
// degrees-minutes spellings fail the range check and are dropped rather than misread.
func iso6709(value string) *Location {
	match := iso6709Pattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return nil
	}
	latitude, errLat := strconv.ParseFloat(match[1], 64)
	longitude, errLon := strconv.ParseFloat(match[2], 64)
	if errLat != nil || errLon != nil || !validCoordinates(latitude, longitude) {
		return nil
	}
	location := &Location{Latitude: latitude, Longitude: longitude}
	if match[3] != "" {
		if altitude, err := strconv.ParseFloat(match[3], 64); err == nil {
			location.Altitude = &altitude
		}
	}
	return location
}
