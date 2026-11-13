// Package mediameta reads the capture facts a photo or video carries inside its own
// bytes: when and where it was taken, on what device, at what size.
//
// It never needs the whole file. Every container this package understands keeps those
// facts in a small, addressable region (a JPEG APP1 segment, an HEIF item, the movie
// header of an MP4), so a parse is a handful of ranged reads whatever the file size. That
// is what makes parsing on demand affordable for bytes that never crossed this service:
// a 4 GB recording costs the same few requests as a 2 MB photo.
//
// The result is derived data. It can always be recomputed from the file, so callers may
// cache it and may throw the cache away.
package mediameta

import (
	"context"
	"errors"
	"path"
	"strings"
)

// Version identifies the parser's output for one input. Bump it whenever a change makes
// the same bytes produce a different Info, so cached results from an older parser stop
// being served instead of hiding the improvement until the file changes.
const Version = 1

// Info is what a file says about its own capture. Every field is nil when the file does
// not state it; nothing here is inferred from the name, the index or the upload.
type Info struct {
	// CapturedAt is ISO 8601 in one of three shapes, because files record time in three
	// ways and collapsing them would invent precision:
	//   "2024-05-01T10:20:30"       wall clock where it was taken, zone not recorded
	//   "2024-05-01T10:20:30+08:00" wall clock plus the recorded offset
	//   "2024-05-01T02:20:30Z"      an instant only; the local zone was not recorded
	CapturedAt *string   `json:"capturedAt"`
	Location   *Location `json:"location"`
	Make       *string   `json:"make"`
	Model      *string   `json:"model"`
	Lens       *string   `json:"lens"`
	// Width and Height are the displayed size: a stored rotation is already applied.
	Width           *int     `json:"width"`
	Height          *int     `json:"height"`
	DurationSeconds *float64 `json:"durationSeconds"`
	FNumber         *float64 `json:"fNumber"`
	ExposureSeconds *float64 `json:"exposureSeconds"`
	ISO             *int     `json:"iso"`
	FocalLength     *float64 `json:"focalLength"`
	FocalLength35mm *int     `json:"focalLength35mm"`
}

// Location is WGS 84 decimal degrees; Altitude is meters above sea level.
type Location struct {
	Latitude  float64  `json:"latitude"`
	Longitude float64  `json:"longitude"`
	Altitude  *float64 `json:"altitude"`
}

// Empty reports that the file stated nothing this package reads.
func (i Info) Empty() bool {
	return i == Info{}
}

// Source is ranged access to one file. ReadAt returns exactly length bytes; the caller
// guarantees 0 <= offset and offset+length <= Size().
type Source interface {
	Size() int64
	ReadAt(ctx context.Context, offset int64, length int) ([]byte, error)
}

// supportedExtensions are the containers Parse understands. The check runs before any
// read, so a document or an archive never costs a storage request.
var supportedExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".jpe": true,
	".heic": true, ".heif": true, ".hif": true, ".avif": true,
	".tif": true, ".tiff": true, ".dng": true, ".nef": true, ".arw": true, ".cr2": true,
	".mp4": true, ".m4v": true, ".mov": true, ".qt": true, ".3gp": true, ".3g2": true,
}

var supportedMediaTypes = map[string]bool{
	"image/jpeg": true, "image/heic": true, "image/heif": true, "image/avif": true, "image/tiff": true,
	"video/mp4": true, "video/quicktime": true, "video/3gpp": true, "video/3gpp2": true, "video/x-m4v": true,
}

// Supported reports whether a file with this name or media type is worth a parse. The
// container is still identified from its first bytes; this only decides whether to look.
func Supported(name, mediaType string) bool {
	if supportedExtensions[strings.ToLower(path.Ext(name))] {
		return true
	}
	kind, _, _ := strings.Cut(strings.ToLower(mediaType), ";")
	return supportedMediaTypes[strings.TrimSpace(kind)]
}

// Parse reads what the file states about its capture. A file that is malformed, of an
// unknown container, or larger than the read budget is not an error: it yields whatever
// was established before the parser stopped, possibly nothing. The only error is a failed
// read, because that says nothing about the file and must not be cached as "no metadata".
func Parse(ctx context.Context, src Source) (Info, error) {
	var info Info
	r := &window{ctx: ctx, src: src, size: src.Size(), blocks: map[int64][]byte{}}
	err := parse(r, &info)
	var stop stopError
	if err != nil && !errors.As(err, &stop) {
		return Info{}, err
	}
	return info, nil
}

func parse(r *window, info *Info) error {
	if r.size < 12 {
		return nil
	}
	head, err := r.bytes(0, 12)
	if err != nil {
		return err
	}
	switch {
	case head[0] == 0xFF && head[1] == 0xD8:
		return parseJPEG(r, info)
	case firstBox[string(head[4:8])]:
		return parseISOBMFF(r, info)
	case string(head[:4]) == "II*\x00" || string(head[:4]) == "MM\x00*":
		// TIFF and the raw formats built on it. Their directories may point anywhere in
		// the file; the first block is where cameras put the ones read here, and an
		// offset beyond it is skipped rather than chased.
		n := r.size
		if n > blockSize {
			n = blockSize
		}
		data, err := r.bytes(0, int(n))
		if err != nil {
			return err
		}
		parseTIFF(data, info)
	}
	return nil
}

// firstBox lists the box types a movie or HEIF file can open with. ISO files start with
// "ftyp"; classic QuickTime movies, which phones still wrote for years, have none and
// start with a placeholder, the media data or the movie box itself.
var firstBox = map[string]bool{"ftyp": true, "wide": true, "mdat": true, "moov": true, "free": true, "skip": true, "pnot": true}

// stopError ends a parse without failing it: the file is malformed or asks for more
// reads than one parse may spend.
type stopError string

func (e stopError) Error() string { return string(e) }

const (
	errMalformed = stopError("malformed media container")
	errBudget    = stopError("media metadata read budget exhausted")
	// errItemLocations names a refused HEIF item location box: it declares more entries
	// or extents than the bytes that describe them can hold, so it is refused as
	// malformed instead of multiplying a zero-width record by a 16-bit count. The
	// reason is stated because this is the one place where "malformed" is also the only
	// thing standing between a 400 KB file and a request that never returns.
	errItemLocations = stopError("refused the HEIF item location box: it declares more entries or extents than its own bytes can describe")
)

const (
	// blockSize is the unit of one ranged read. 256 KiB holds everything a photo needs
	// (a JPEG APP1 segment is at most 64 KiB and precedes the image data; an HEIF meta
	// box and its Exif item sit at the front) and the whole movie header of a typical
	// phone clip, so the common cases are one or two requests.
	blockSize = 256 << 10
	// maxBlocks bounds one parse. A video needs the first block, the start of its movie
	// header, one block per track whose header lies elsewhere (phone recordings carry up
	// to about six tracks) and the user-data tail: roughly ten in the worst realistic
	// layout. Measured on 3,827 phone recordings up to 2.5 GB: at most 4. A 21 GB,
	// 50-minute recording: 6. 16 leaves headroom and caps a parse at 4 MiB transferred
	// for any file.
	maxBlocks = 16
	// maxRegion bounds one metadata region copied out of the window (an Exif payload, an
	// HEIF meta box, a movie user-data box). Real ones are tens of kilobytes; a larger
	// claim is a malformed length, not metadata.
	maxRegion = 1 << 20
)

// window serves random access over a Source from aligned cached blocks, so parsers can
// follow offsets without thinking about request boundaries.
type window struct {
	ctx    context.Context
	src    Source
	size   int64
	blocks map[int64][]byte
}

func (w *window) block(index int64) ([]byte, error) {
	if data, ok := w.blocks[index]; ok {
		return data, nil
	}
	if len(w.blocks) >= maxBlocks {
		return nil, errBudget
	}
	start := index * blockSize
	length := w.size - start
	if length > blockSize {
		length = blockSize
	}
	data, err := w.src.ReadAt(w.ctx, start, int(length))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != length {
		return nil, errors.New("media source returned a short read")
	}
	w.blocks[index] = data
	return data, nil
}

// bytes returns exactly n bytes at offset. A range outside the file is a malformed
// container, never a read error.
func (w *window) bytes(offset int64, n int) ([]byte, error) {
	if offset < 0 || n < 0 || n > maxRegion || offset > w.size-int64(n) {
		return nil, errMalformed
	}
	out := make([]byte, 0, n)
	for len(out) < n {
		position := offset + int64(len(out))
		data, err := w.block(position / blockSize)
		if err != nil {
			return nil, err
		}
		within := int(position % blockSize)
		take := n - len(out)
		if take > len(data)-within {
			take = len(data) - within
		}
		out = append(out, data[within:within+take]...)
	}
	return out, nil
}

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }
func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
func be64(b []byte) uint64 { return uint64(be32(b))<<32 | uint64(be32(b[4:])) }

// text cleans a string taken from a file: these fields are padded with NULs and spaces,
// and nothing guarantees they are UTF-8.
func text(b []byte) *string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	s := strings.TrimSpace(strings.ToValidUTF8(string(b), ""))
	if s == "" {
		return nil
	}
	if len(s) > 200 {
		s = strings.ToValidUTF8(s[:200], "")
	}
	return &s
}

func validCoordinates(latitude, longitude float64) bool {
	// 0,0 is what devices write when they have no fix; no photo is taken there.
	return latitude >= -90 && latitude <= 90 && longitude >= -180 && longitude <= 180 && !(latitude == 0 && longitude == 0)
}
