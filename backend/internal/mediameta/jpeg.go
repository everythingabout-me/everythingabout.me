package mediameta

// parseJPEG walks the marker segments before the image data. EXIF is an APP1 segment and
// the pixel size is in the frame header; both precede the first scan, so the walk reads
// segment headers only and never touches the compressed image.
func parseJPEG(r *window, info *Info) error {
	var (
		offset        = int64(2)
		quarterTurn   bool
		width, height int64
	)
	// A JPEG has a few dozen segments before its scan. The bound only stops a file that
	// is all padding from walking to the read budget one byte at a time.
walk:
	for segments := 0; segments < 256; segments++ {
		head, err := r.bytes(offset, 4)
		if err != nil {
			return finishJPEG(info, width, height, quarterTurn, err)
		}
		if head[0] != 0xFF {
			break
		}
		marker := head[1]
		switch {
		case marker == 0xFF: // fill byte before a marker
			offset++
			continue
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD8): // no payload
			offset += 2
			continue
		case marker == 0xDA || marker == 0xD9: // image data or end: nothing further
			break walk
		}
		length := int64(be16(head[2:]))
		if length < 2 {
			break
		}
		payload := offset + 4
		size := int(length - 2)
		switch {
		case marker == 0xE1 && size > 6:
			data, err := r.bytes(payload, size)
			if err != nil {
				return finishJPEG(info, width, height, quarterTurn, err)
			}
			if string(data[:6]) == "Exif\x00\x00" {
				quarterTurn = parseTIFF(data[6:], info)
			}
		// Start-of-frame markers, except the three in that range that are tables.
		case marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC && size >= 5:
			data, err := r.bytes(payload, 5)
			if err != nil {
				return finishJPEG(info, width, height, quarterTurn, err)
			}
			height, width = int64(be16(data[1:])), int64(be16(data[3:]))
		}
		offset = payload + int64(size)
	}
	return finishJPEG(info, width, height, quarterTurn, nil)
}

// The frame header is the truth about the stored pixels; EXIF's own size tags go stale
// when an editor resizes the image without rewriting them.
func finishJPEG(info *Info, width, height int64, quarterTurn bool, err error) error {
	if width > 0 && height > 0 {
		setDimensions(info, width, height, quarterTurn)
	}
	return err
}
