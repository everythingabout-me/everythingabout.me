package mediameta

import (
	"context"
	"errors"
	"testing"
	"time"
)

// oversizedHEIF builds the file that made the old item-location walk spin: every
// declared entry carries a zero-width record (no offset, length, base or index fields)
// and declares the maximum 65,535 extents. Each such entry costs six bytes, so about
// 393 KB of iloc declares 65,535 entries × 65,535 extents = 4.3 billion iterations,
// none of which read or advance anything.
//
// The rest of the file is a well-formed HEIF shell: an Exif item so the walk runs at
// all, and one ispe property so the declared dimensions are known before the refusal.
func oversizedHEIF(t *testing.T) []byte {
	t.Helper()
	ftyp := atom("ftyp", []byte("heic\x00\x00\x00\x00mif1heic"))
	// version 0, offset/length widths 0, base/index widths 0, entry count 65,535.
	header := []byte{0, 0, 0, 0, 0x00, 0x00}
	const entries = 0xFFFF
	record := []byte{0, 0, 0, 0, 0xFF, 0xFF} // id 0, reference 0, extents 65,535
	payload := make([]byte, 0, 2+entries*len(record))
	payload = append(payload, header...)
	payload = append(payload, u16(entries)...)
	for i := 0; i < entries; i++ {
		payload = append(payload, record...)
	}
	meta := atom("meta", u32(0),
		atom("iinf", u32(0), u16(1),
			atom("infe", []byte{2, 0, 0, 0}, u16(7), u16(0), []byte("Exif"), []byte{0})),
		atom("iloc", payload),
		atom("iprp", atom("ipco", atom("ispe", u32(0), u32(4032), u32(3024)))),
	)
	return append(ftyp, meta...)
}

// An item location box may not declare more work than its bytes describe. The refusal
// is the named error, the declared dimensions survive it, and the parse returns instead
// of walking billions of empty extents.
func TestHEIFItemLocationsAreBoundedByTheBytesThatDescribeThem(t *testing.T) {
	file := oversizedHEIF(t)
	src := whole(file)
	if len(file) < 380<<10 || len(file) > 400<<10 {
		t.Fatalf("fixture is %d bytes, want about 400 KB", len(file))
	}

	r := &window{ctx: context.Background(), src: src, size: src.Size(), blocks: map[int64][]byte{}}
	info := &Info{}
	start := time.Now()
	err := parseISOBMFF(r, info)
	elapsed := time.Since(start)

	if !errors.Is(err, errItemLocations) {
		t.Fatalf("parse error = %v, want the named %q refusal", err, errItemLocations)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the bounded walk took %s; a real bound is one comparison, not a second", elapsed)
	}
	if info.Width == nil || info.Height == nil || *info.Width != 4032 || *info.Height != 3024 {
		t.Fatalf("the declared dimensions were lost by the refusal: %+v", info)
	}

	// Parse's contract is unchanged: a malformed file yields what it established and no
	// error, so a request for this file does not fail — it just refuses the Exif item.
	parsed, err := Parse(context.Background(), whole(file))
	if err != nil {
		t.Fatalf("Parse reported the malformed file as a read failure: %v", err)
	}
	if parsed.Width == nil || *parsed.Width != 4032 {
		t.Fatalf("Parse lost the declared dimensions: %+v", parsed)
	}
}

// A well-formed iloc still resolves, so the budget did not turn a real camera file into
// a refusal.
func TestHEIFItemLocationBudgetAcceptsARealLayout(t *testing.T) {
	src := whole(heic(phoneExif(1), true))
	r := &window{ctx: context.Background(), src: src, size: src.Size(), blocks: map[int64][]byte{}}
	info := &Info{}
	if err := parseISOBMFF(r, info); err != nil {
		t.Fatalf("a real HEIC was refused: %v", err)
	}
	if info.CapturedAt == nil || info.Make == nil {
		t.Fatalf("a real HEIC lost its Exif facts: %+v", info)
	}
}
