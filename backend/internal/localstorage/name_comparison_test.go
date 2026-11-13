package localstorage

import (
	"testing"

	"everythingabout.me/backend/internal/nameprobe"
)

// The Local driver's native equivalence decides whether a name is the same
// file for Stat, collision checks and rename. It is the fold-only pipeline.
func TestDriverKeyUsesLegacyKey(t *testing.T) {
	factory, cfg := fixture(t, Options{CaseSensitive: false})
	driver := bind(t, factory, cfg).(*driver)
	nameprobe.CheckKey(t, "localstorage.driver.key", nameprobe.LegacyPipeline, false, driver.key)
}
