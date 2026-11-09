package sizeformat

import "testing"

// These pairs are the contract shared with web/src/pages/sizeFormat.ts and listed in
// docs/design/size-units.md; changing one side means changing all three together.
func TestBytes(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{137, "137 B"},
		{999, "999 B"},
		{1000, "1 KB"},
		{1001, "1 KB"},
		{383000, "383 KB"},
		{27800000, "27.8 MB"},
		{999900, "1 MB"},
		{999950, "1 MB"},
		{1000000, "1 MB"},
		{2120000000, "2.12 GB"},
		{21253968461, "21.25 GB"},
		{3900000000, "3.9 GB"},
		{14257400000, "14.26 GB"},
		{1000000000000, "1 TB"},
		{1500000000000, "1.5 TB"},
		{999999999999, "1 TB"},
		{-1, "0 B"},
	}
	for _, c := range cases {
		if got := Bytes(c.bytes); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.bytes, got, c.want)
		}
	}
}

// The cap and limit copy is generated from these, so the constant must print as the round
// number it is documented as.
func TestDecimalLimitsPrintAsWritten(t *testing.T) {
	if got := Bytes(48 * 1000 * 1000 * 1000); got != "48 GB" {
		t.Errorf("file cap: got %q, want %q", got, "48 GB")
	}
	if got := Bytes(32 * 1000); got != "32 KB" {
		t.Errorf("html cap: got %q, want %q", got, "32 KB")
	}
}
