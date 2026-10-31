//go:build !darwin && !linux

package localstorage

import "os"

// Additional platforms need their own tested local-filesystem admission proof.
func stagedLocalFilesystem(root *os.Root) (bool, error) { return false, nil }
