package localstorage

import (
	"testing"

	"everythingabout.me/backend/internal/nameprobe"
)

// The Local driver's native equivalence decides whether a name is the same
// file for Stat, collision checks and rename. It must be the canonical key.
func TestDriverKeyUsesReferenceKey(t *testing.T) {
	factory, cfg := fixture(t, Options{CaseSensitive: false})
	driver := bind(t, factory, cfg).(*driver)
	nameprobe.CheckKey(t, "localstorage.driver.key", nameprobe.ReferencePipeline, false, driver.key)
}
