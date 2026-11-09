// Package sizeformat renders byte counts for humans the way the rest of the product does:
// decimal units, so 1 KB is 1000 bytes and never KiB. macOS Finder and `ls -h` print sizes
// the same way, which matters because users compare the number here with the number in
// their file manager.
//
// The exact digits, trailing-zero trimming and unit-boundary promotion are part of the
// contract; docs/design/size-units.md holds the shared table of examples that
// web/src/pages/sizeFormat.ts must keep matching.
//
// Raw byte counts stay integers on the wire, in headers, in logs and in diagnostics; this
// package is only for text a person reads.
package sizeformat

import (
	"strconv"
	"strings"
)

// units follow Finder's per-unit precision: KB has no decimals, MB one, GB and TB two.
var units = []struct {
	label    string
	decimals int
}{
	{"KB", 0},
	{"MB", 1},
	{"GB", 2},
	{"TB", 2},
}

// Bytes renders a byte count as text, for example "21.25 GB" for a 21.25 GB file.
// A count below 1000 stays exact bytes ("137 B"); values at or above 1000 TB keep the TB
// unit rather than inventing a larger one.
func Bytes(n int64) string {
	if n < 1000 {
		if n < 0 {
			n = 0
		}
		return strconv.FormatInt(n, 10) + " B"
	}
	value := float64(n) / 1000
	unit := 0
	for value >= 1000 && unit < len(units)-1 {
		value /= 1000
		unit++
	}
	text := format(value, units[unit].decimals)
	// Rounding can push a value over its unit boundary: 999.9 KB would read "1000 KB".
	// Promote it instead of printing four digits after the decimals are gone.
	for {
		printed, err := strconv.ParseFloat(text, 64)
		if err != nil || printed < 1000 || unit == len(units)-1 {
			break
		}
		value /= 1000
		unit++
		text = format(value, units[unit].decimals)
	}
	return text + " " + units[unit].label
}

func format(value float64, decimals int) string {
	text := strconv.FormatFloat(value, 'f', decimals, 64)
	// Drop trailing zeros the way Finder does: 2.00 GB -> "2 GB", 3.90 GB -> "3.9 GB".
	if !strings.Contains(text, ".") {
		return text
	}
	return strings.TrimSuffix(strings.TrimRight(text, "0"), ".")
}
