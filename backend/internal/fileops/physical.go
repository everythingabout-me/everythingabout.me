package fileops

// PhysicalDelta charges durable native bytes independently of current-key logical
// size. Version-retaining backends keep old generations after replace/delete;
// only independently verified history retirement may later credit those bytes.
// Arguments are nonnegative admitted sizes, so their difference cannot overflow.
func PhysicalDelta(root Root, newBytes, oldBytes int64) int64 {
	if root.RetainsVersions {
		return newBytes
	}
	return newBytes - oldBytes
}

// PhysicalReservation includes both a replayable full ingress/copy spool and the
// possible final native generation on retaining object backends. Local staging
// publishes the same inode and needs one allocation, not a second copy.
func PhysicalReservation(root Root, newBytes int64) (int64, error) {
	if newBytes < 0 {
		return 0, E("invalid_request", nil)
	}
	if !root.RetainsVersions {
		return newBytes, nil
	}
	reserved, ok := addOK(newBytes, newBytes)
	if !ok {
		return 0, E("reservation_exceeded", nil)
	}
	return reserved, nil
}
