package mediameta

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

// sparseSource is a file of any declared size made of a few real regions; everything
// between them reads as zeros. It counts requests so a test can state what a parse costs.
type sparseSource struct {
	size    int64
	regions []sparseRegion
	reads   int
	bytes   int64
	fail    error
}

type sparseRegion struct {
	offset int64
	data   []byte
}

func (s *sparseSource) Size() int64 { return s.size }
func (s *sparseSource) ReadAt(_ context.Context, offset int64, length int) ([]byte, error) {
	s.reads++
	s.bytes += int64(length)
	if s.fail != nil {
		return nil, s.fail
	}
	out := make([]byte, length)
	for _, region := range s.regions {
		from, to := region.offset, region.offset+int64(len(region.data))
		if to <= offset || from >= offset+int64(length) {
			continue
		}
		start, end := max(from, offset), min(to, offset+int64(length))
		copy(out[start-offset:end-offset], region.data[start-from:end-from])
	}
	return out, nil
}

func whole(data []byte) *sparseSource {
	return &sparseSource{size: int64(len(data)), regions: []sparseRegion{{data: data}}}
}

// ---------- fixture builders ----------

type field struct {
	tag, kind uint16
	count     uint32
	value     []byte
}

func ascii(tag uint16, s string) field {
	return field{tag, 2, uint32(len(s) + 1), append([]byte(s), 0)}
}
func short(tag uint16, v uint16) field {
	return field{tag, 3, 1, binary.LittleEndian.AppendUint16(nil, v)}
}
func long(tag uint16, v uint32) field {
	return field{tag, 4, 1, binary.LittleEndian.AppendUint32(nil, v)}
}
func rational(tag uint16, pairs ...uint32) field {
	var value []byte
	for _, v := range pairs {
		value = binary.LittleEndian.AppendUint32(value, v)
	}
	return field{tag, 5, uint32(len(pairs) / 2), value}
}

// directory lays one IFD out at start, with long values placed right after it.
func directory(start uint32, fields []field) []byte {
	out := binary.LittleEndian.AppendUint16(nil, uint16(len(fields)))
	extra := start + 2 + uint32(len(fields))*12 + 4
	var tail []byte
	for _, f := range fields {
		out = binary.LittleEndian.AppendUint16(out, f.tag)
		out = binary.LittleEndian.AppendUint16(out, f.kind)
		out = binary.LittleEndian.AppendUint32(out, f.count)
		if len(f.value) <= 4 {
			out = append(out, append(append([]byte{}, f.value...), make([]byte, 4-len(f.value))...)...)
			continue
		}
		out = binary.LittleEndian.AppendUint32(out, extra+uint32(len(tail)))
		tail = append(tail, f.value...)
	}
	out = append(out, 0, 0, 0, 0)
	return append(out, tail...)
}

// exifBlock is a little-endian TIFF with a root, an Exif and a GPS directory.
func exifBlock(root, exif, gps []field) []byte {
	build := func(exifAt, gpsAt uint32) ([]byte, []byte, []byte) {
		all := append(append([]field{}, root...), long(0x8769, exifAt), long(0x8825, gpsAt))
		return directory(8, all), directory(exifAt, exif), directory(gpsAt, gps)
	}
	first, second, _ := build(0, 0)
	exifAt := 8 + uint32(len(first))
	gpsAt := exifAt + uint32(len(second))
	first, second, third := build(exifAt, gpsAt)
	out := append([]byte("II*\x00"), 8, 0, 0, 0)
	return append(append(append(out, first...), second...), third...)
}

func phoneExif(orientation uint16) []byte {
	return exifBlock(
		[]field{ascii(0x010F, "Apple"), ascii(0x0110, "iPhone 15 Pro"), short(0x0112, orientation), ascii(0x0132, "2024:06:01 09:00:00")},
		[]field{
			rational(0x829A, 1, 120), rational(0x829D, 178, 100), short(0x8827, 80),
			ascii(0x9003, "2024:05:01 10:20:30"), ascii(0x9011, "+08:00"),
			rational(0x920A, 6765, 1000), long(0xA002, 4032), long(0xA003, 3024), short(0xA405, 24),
			ascii(0xA434, "iPhone 15 Pro back triple camera 6.765mm f/1.78"),
		},
		[]field{
			ascii(0x0001, "N"), rational(0x0002, 31, 1, 13, 1, 4944, 100),
			ascii(0x0003, "E"), rational(0x0004, 121, 1, 28, 1, 2532, 100),
			{0x0005, 1, 1, []byte{0}}, rational(0x0006, 1234, 100),
		},
	)
}

func segment(marker byte, payload []byte) []byte {
	out := []byte{0xFF, marker}
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)+2))
	return append(out, payload...)
}

func jpeg(exif []byte, width, height uint16) []byte {
	out := []byte{0xFF, 0xD8}
	out = append(out, segment(0xE0, []byte("JFIF\x00\x01\x02\x00\x00\x01\x00\x01\x00\x00"))...)
	if exif != nil {
		out = append(out, segment(0xE1, append([]byte("Exif\x00\x00"), exif...))...)
	}
	out = append(out, segment(0xDB, make([]byte, 65))...)
	frame := []byte{8}
	frame = binary.BigEndian.AppendUint16(frame, height)
	frame = binary.BigEndian.AppendUint16(frame, width)
	out = append(out, segment(0xC0, append(frame, 3, 1, 0x22, 0, 2, 0x11, 1, 3, 0x11, 1))...)
	out = append(out, segment(0xDA, make([]byte, 10))...)
	return append(out, make([]byte, 4096)...)
}

func atom(kind string, payload ...[]byte) []byte {
	size := 8
	for _, p := range payload {
		size += len(p)
	}
	out := binary.BigEndian.AppendUint32(nil, uint32(size))
	out = append(out, kind...)
	for _, p := range payload {
		out = append(out, p...)
	}
	return out
}

func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func heic(exif []byte, rotated bool) []byte {
	item := append(append(u32(6), "Exif\x00\x00"...), exif...)
	ftyp := atom("ftyp", []byte("heic\x00\x00\x00\x00mif1heic"))
	properties := [][]byte{
		atom("ispe", u32(0), u32(320), u32(240)),
		atom("ispe", u32(0), u32(4032), u32(3024)),
	}
	if rotated {
		properties = append(properties, atom("irot", []byte{3}))
	}
	meta := func(itemOffset uint32) []byte {
		return atom("meta", u32(0),
			atom("hdlr", u32(0), u32(0), []byte("pict"), make([]byte, 13)),
			atom("pitm", u32(0), u16(1)),
			atom("iinf", u32(0), u16(2),
				atom("infe", []byte{2, 0, 0, 0}, u16(1), u16(0), []byte("grid"), []byte{0}),
				atom("infe", []byte{2, 0, 0, 0}, u16(7), u16(0), []byte("Exif"), []byte{0})),
			// version 1, 4-byte offsets and lengths, no base offset
			atom("iloc", []byte{1, 0, 0, 0}, []byte{0x44, 0x00}, u16(2),
				u16(1), u16(0), u16(0), u16(1), u32(itemOffset+uint32(len(item))), u32(16),
				u16(7), u16(0), u16(0), u16(1), u32(itemOffset), u32(uint32(len(item)))),
			atom("iprp", atom("ipco", properties...)),
		)
	}
	itemOffset := uint32(len(ftyp) + len(meta(0)) + 8)
	return append(append(ftyp, meta(itemOffset)...), atom("mdat", item, make([]byte, 2048))...)
}

func trackHeader(width, height uint32, rotated bool) []byte {
	matrix := []uint32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
	if rotated {
		matrix = []uint32{0, 0x10000, 0, 0xFFFF0000, 0, 0, 0, 0, 0x40000000}
	}
	payload := append(u32(0), make([]byte, 36)...)
	for _, v := range matrix {
		payload = append(payload, u32(v)...)
	}
	payload = append(payload, u32(width<<16)...)
	return atom("tkhd", payload, u32(height<<16))
}

func keyed(pairs ...string) []byte {
	keys := append(u32(0), u32(uint32(len(pairs)/2))...)
	var values []byte
	for i := 0; i < len(pairs); i += 2 {
		keys = append(keys, atom("mdta", []byte(pairs[i]))...)
		values = append(values, atom(string(u32(uint32(i/2+1))), atom("data", u32(1), u32(0), []byte(pairs[i+1])))...)
	}
	return atom("meta", atom("hdlr", make([]byte, 24)), atom("keys", keys), atom("ilst", values))
}

func movieHeader(created, timescale, duration uint32) []byte {
	return atom("mvhd", u32(0), u32(created), u32(created), u32(timescale), u32(duration), make([]byte, 80))
}

// ---------- tests ----------

func value[T any](t *testing.T, name string, p *T) T {
	t.Helper()
	if p == nil {
		t.Fatalf("%s was not read", name)
	}
	return *p
}

func near(a, b float64) bool { return a-b < 1e-6 && b-a < 1e-6 }

func checkPhoneExif(t *testing.T, info Info) {
	t.Helper()
	if got := value(t, "capturedAt", info.CapturedAt); got != "2024-05-01T10:20:30+08:00" {
		t.Errorf("capturedAt = %q, want the original time with its recorded offset", got)
	}
	location := value(t, "location", info.Location)
	if !near(location.Latitude, 31+13.0/60+49.44/3600) || !near(location.Longitude, 121+28.0/60+25.32/3600) {
		t.Errorf("location = %v, %v", location.Latitude, location.Longitude)
	}
	if !near(value(t, "altitude", location.Altitude), 12.34) {
		t.Errorf("altitude = %v", *location.Altitude)
	}
	if value(t, "make", info.Make) != "Apple" || value(t, "model", info.Model) != "iPhone 15 Pro" {
		t.Errorf("device = %q %q", *info.Make, *info.Model)
	}
	if got := value(t, "lens", info.Lens); got != "iPhone 15 Pro back triple camera 6.765mm f/1.78" {
		t.Errorf("lens = %q", got)
	}
	if !near(value(t, "exposure", info.ExposureSeconds), 1.0/120) || !near(value(t, "fNumber", info.FNumber), 1.78) ||
		value(t, "iso", info.ISO) != 80 || !near(value(t, "focalLength", info.FocalLength), 6.765) || value(t, "focalLength35mm", info.FocalLength35mm) != 24 {
		t.Errorf("exposure facts: %v %v %v %v %v", *info.ExposureSeconds, *info.FNumber, *info.ISO, *info.FocalLength, *info.FocalLength35mm)
	}
}

func TestJPEGReadsExifAndFrameSizeInOneRequest(t *testing.T) {
	src := whole(jpeg(phoneExif(6), 4032, 3024))
	info, err := Parse(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	checkPhoneExif(t, info)
	// Orientation 6 shows the 4032x3024 frame as portrait.
	if value(t, "width", info.Width) != 3024 || value(t, "height", info.Height) != 4032 {
		t.Errorf("displayed size = %dx%d, want 3024x4032", *info.Width, *info.Height)
	}
	if src.reads != 1 {
		t.Errorf("a small JPEG took %d requests, want 1", src.reads)
	}
}

func TestJPEGWithoutExifStillReportsItsSize(t *testing.T) {
	info, err := Parse(context.Background(), whole(jpeg(nil, 800, 600)))
	if err != nil {
		t.Fatal(err)
	}
	if value(t, "width", info.Width) != 800 || value(t, "height", info.Height) != 600 {
		t.Errorf("size = %dx%d", *info.Width, *info.Height)
	}
	if info.CapturedAt != nil || info.Location != nil || info.Make != nil {
		t.Errorf("a JPEG without EXIF produced capture facts: %+v", info)
	}
}

func TestExifTimeShapes(t *testing.T) {
	for _, test := range []struct{ value, offset, want string }{
		{"2024:05:01 10:20:30", "", "2024-05-01T10:20:30"},
		{"2024:05:01 10:20:30", "-07:00", "2024-05-01T10:20:30-07:00"},
		{"2024:05:01 10:20:30", "bogus!", "2024-05-01T10:20:30"},
		{"0000:00:00 00:00:00", "", ""},
		{"2024-05-01 10:20:30", "", ""},
		{"    :  :     :  :  ", "", ""},
	} {
		got := exifTime([]byte(test.value+"\x00"), []byte(test.offset))
		if (got == nil) != (test.want == "") || (got != nil && *got != test.want) {
			t.Errorf("exifTime(%q, %q) = %v, want %q", test.value, test.offset, got, test.want)
		}
	}
}

func TestGPSWithoutAFixIsNotALocation(t *testing.T) {
	exif := exifBlock(nil, nil, []field{
		ascii(0x0001, "N"), rational(0x0002, 0, 1, 0, 1, 0, 1),
		ascii(0x0003, "E"), rational(0x0004, 0, 1, 0, 1, 0, 1),
	})
	info, err := Parse(context.Background(), whole(jpeg(exif, 10, 10)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Location != nil {
		t.Errorf("0,0 was reported as a location: %+v", info.Location)
	}
}

func TestSouthWestHemispheres(t *testing.T) {
	exif := exifBlock(nil, nil, []field{
		ascii(0x0001, "S"), rational(0x0002, 33, 1, 51, 1, 0, 1),
		ascii(0x0003, "W"), rational(0x0004, 70, 1, 40, 1, 0, 1),
		{0x0005, 1, 1, []byte{1}}, rational(0x0006, 50, 1),
	})
	info, _ := Parse(context.Background(), whole(jpeg(exif, 10, 10)))
	location := value(t, "location", info.Location)
	if !near(location.Latitude, -33.85) || !near(location.Longitude, -(70+40.0/60)) || !near(value(t, "altitude", location.Altitude), -50) {
		t.Errorf("location = %+v altitude %v", location, *location.Altitude)
	}
}

func TestHEICReadsExifItemAndItemProperties(t *testing.T) {
	// The EXIF orientation says upright; the item rotation property says portrait, and
	// that property is what HEIF readers honor.
	src := whole(heic(phoneExif(1), true))
	info, err := Parse(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	checkPhoneExif(t, info)
	if value(t, "width", info.Width) != 3024 || value(t, "height", info.Height) != 4032 {
		t.Errorf("displayed size = %dx%d, want 3024x4032", *info.Width, *info.Height)
	}
	if src.reads != 1 {
		t.Errorf("a small HEIC took %d requests, want 1", src.reads)
	}

	info, _ = Parse(context.Background(), whole(heic(phoneExif(1), false)))
	if *info.Width != 4032 || *info.Height != 3024 {
		t.Errorf("unrotated size = %dx%d", *info.Width, *info.Height)
	}
}

func TestPhoneMovieWithHeaderAfterGigabytesOfSamples(t *testing.T) {
	const samples = int64(4) << 30
	ftyp := atom("ftyp", []byte("qt  \x00\x00\x00\x00qt  "))
	mdat := append(append(u32(1), "mdat"...), u64(uint64(samples))...)
	moov := atom("moov",
		movieHeader(uint32(1714530030+movieEpochOffset), 600, 600*95),
		atom("trak", trackHeader(1920, 1080, true), atom("mdia", make([]byte, 64))),
		atom("trak", trackHeader(0, 0, false), atom("mdia", make([]byte, 64))),
		keyed(
			"com.apple.quicktime.creationdate", "2024-05-01T10:20:30+0800",
			"com.apple.quicktime.location.ISO6709", "+31.2304+121.4737+012.340/",
			"com.apple.quicktime.make", "Apple",
			"com.apple.quicktime.model", "iPhone 15 Pro",
		),
	)
	head := append(ftyp, mdat...)
	moovAt := int64(len(ftyp)) + samples
	src := &sparseSource{size: moovAt + int64(len(moov)), regions: []sparseRegion{{0, head}, {moovAt, moov}}}

	info, err := Parse(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if got := value(t, "capturedAt", info.CapturedAt); got != "2024-05-01T10:20:30+08:00" {
		t.Errorf("capturedAt = %q, want the keyed creation date over the header's UTC instant", got)
	}
	location := value(t, "location", info.Location)
	if !near(location.Latitude, 31.2304) || !near(location.Longitude, 121.4737) || !near(value(t, "altitude", location.Altitude), 12.34) {
		t.Errorf("location = %+v", location)
	}
	if *info.Make != "Apple" || *info.Model != "iPhone 15 Pro" {
		t.Errorf("device = %q %q", *info.Make, *info.Model)
	}
	if !near(value(t, "duration", info.DurationSeconds), 95) {
		t.Errorf("duration = %v", *info.DurationSeconds)
	}
	if value(t, "width", info.Width) != 1080 || value(t, "height", info.Height) != 1920 {
		t.Errorf("displayed size = %dx%d, want the rotated 1080x1920", *info.Width, *info.Height)
	}
	// The whole point: the cost does not depend on the 4 GiB between the two regions.
	if src.reads > 3 || src.bytes > 3*blockSize {
		t.Errorf("a 4 GiB movie took %d requests and %d bytes", src.reads, src.bytes)
	}
}

func TestAndroidMovieUsesClassicUserData(t *testing.T) {
	location := "+37.4220-122.0840/"
	xyz := atom("\xa9xyz", u16(uint16(len(location))), u16(0x15c7), []byte(location))
	movie := append(atom("ftyp", []byte("isom\x00\x00\x02\x00isommp42")),
		atom("moov",
			movieHeader(uint32(1714530030+movieEpochOffset), 1000, 12500),
			atom("trak", trackHeader(1280, 720, false)),
			atom("udta", xyz),
		)...)
	movie = append(movie, atom("mdat", make([]byte, 1024))...)
	info, err := Parse(context.Background(), whole(movie))
	if err != nil {
		t.Fatal(err)
	}
	// Only the header's UTC instant is known, so the result is an instant, not a wall clock.
	if got := value(t, "capturedAt", info.CapturedAt); got != "2024-05-01T02:20:30Z" {
		t.Errorf("capturedAt = %q", got)
	}
	place := value(t, "location", info.Location)
	if !near(place.Latitude, 37.4220) || !near(place.Longitude, -122.0840) || place.Altitude != nil {
		t.Errorf("location = %+v", place)
	}
	if !near(*info.DurationSeconds, 12.5) || *info.Width != 1280 || *info.Height != 720 {
		t.Errorf("movie facts: %v %dx%d", *info.DurationSeconds, *info.Width, *info.Height)
	}
}

func TestClassicQuickTimeMovieWithoutFileTypeBox(t *testing.T) {
	movie := append(atom("wide"), atom("mdat", make([]byte, 512))...)
	movie = append(movie, atom("moov", movieHeader(uint32(1714530030+movieEpochOffset), 600, 1200), atom("trak", trackHeader(1920, 1080, false)))...)
	info, err := Parse(context.Background(), whole(movie))
	if err != nil {
		t.Fatal(err)
	}
	if info.DurationSeconds == nil || info.Width == nil || *info.Width != 1920 {
		t.Fatalf("a movie that opens with \"wide\" was not recognized: %+v", info)
	}
}

func TestMovieHeaderWithoutAClockIsNotACaptureTime(t *testing.T) {
	for _, created := range []uint32{0, 1714530030} { // unset, and Unix seconds in a 1904-based field
		movie := append(atom("ftyp", []byte("isom\x00\x00\x02\x00isom")), atom("moov", movieHeader(created, 1000, 1000))...)
		info, _ := Parse(context.Background(), whole(movie))
		if info.CapturedAt != nil {
			t.Errorf("creation value %d became capture time %q", created, *info.CapturedAt)
		}
	}
}

func TestISOTimeKeepsTheRecordedOffset(t *testing.T) {
	for value, want := range map[string]string{
		"2024-05-01T10:20:30+0800":      "2024-05-01T10:20:30+08:00",
		"2024-05-01T10:20:30-07:00":     "2024-05-01T10:20:30-07:00",
		"2024-05-01T10:20:30+0000":      "2024-05-01T10:20:30+00:00",
		"2024-05-01T02:20:30Z":          "2024-05-01T02:20:30Z",
		"2024-05-01T02:20:30.123456Z":   "2024-05-01T02:20:30Z",
		"2024":                          "",
		"2024-05-01":                    "",
		"not a date":                    "",
		"2024-05-01T10:20:30+0800\x00 ": "2024-05-01T10:20:30+08:00",
	} {
		got := isoTime(value)
		if (got == nil) != (want == "") || (got != nil && *got != want) {
			t.Errorf("isoTime(%q) = %v, want %q", value, got, want)
		}
	}
}

func TestISO6709RejectsWhatItCannotRead(t *testing.T) {
	for _, value := range []string{"", "garbage", "+3113.82+12128.42/", "+00.0000+000.0000/", "+91.0+000.0/"} {
		if got := iso6709(value); got != nil {
			t.Errorf("iso6709(%q) = %+v, want nothing", value, got)
		}
	}
}

func TestMalformedFilesAreEmptyNotErrors(t *testing.T) {
	full := heic(phoneExif(1), true)
	cases := map[string][]byte{
		"empty":             {},
		"text":              []byte("just some text, long enough to sniff"),
		"jpeg cut in exif":  jpeg(phoneExif(1), 10, 10)[:60],
		"heic cut in meta":  full[:80],
		"box size overflow": append(atom("ftyp", []byte("heic\x00\x00\x00\x00")), 0xFF, 0xFF, 0xFF, 0xFF, 'm', 'e', 't', 'a', 0, 0, 0, 0),
		"tiff bad offset":   append([]byte("II*\x00"), 0xFF, 0xFF, 0xFF, 0x7F, 0, 0, 0, 0),
	}
	for name, data := range cases {
		info, err := Parse(context.Background(), whole(data))
		if err != nil {
			t.Errorf("%s: malformed input failed the parse: %v", name, err)
		}
		if info.Location != nil || info.CapturedAt != nil {
			t.Errorf("%s: produced facts from a broken file: %+v", name, info)
		}
	}
}

func TestReadFailureIsAnErrorNotAnEmptyResult(t *testing.T) {
	src := whole(jpeg(phoneExif(1), 10, 10))
	src.fail = errors.New("provider unavailable")
	if _, err := Parse(context.Background(), src); err == nil {
		t.Fatal("a failed read was reported as a file without metadata, which a cache would then keep")
	}
}

func TestParseStaysWithinItsReadBudget(t *testing.T) {
	// A fragmented movie: one box per megabyte for a gigabyte, and no header to find.
	const fragment = int64(1) << 20
	src := &sparseSource{size: 1024 * fragment}
	src.regions = append(src.regions, sparseRegion{0, atom("ftyp", []byte("iso5\x00\x00\x00\x00iso5"))})
	first := int64(len(src.regions[0].data))
	for offset := first; offset+fragment <= src.size; offset += fragment {
		src.regions = append(src.regions, sparseRegion{offset, append(u32(uint32(fragment)), "moof"...)})
	}
	info, err := Parse(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Empty() {
		t.Errorf("facts from a file with none: %+v", info)
	}
	if src.reads > maxBlocks {
		t.Errorf("parse made %d requests, budget is %d", src.reads, maxBlocks)
	}
}

func TestSupported(t *testing.T) {
	for name, want := range map[string]bool{
		"IMG_0001.HEIC": true, "a.jpg": true, "clip.MOV": true, "clip.mp4": true, "raw.dng": true,
		"notes.txt": false, "archive.zip": false, "scan.pdf": false, "noext": false,
	} {
		if got := Supported(name, "application/octet-stream"); got != want {
			t.Errorf("Supported(%q) = %v, want %v", name, got, want)
		}
	}
	if !Supported("noext", "image/jpeg") || !Supported("noext", "video/quicktime; codecs=hvc1") {
		t.Error("a supported media type must be enough when the name has no extension")
	}
}
