package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"everythingabout.me/backend/internal/config"
)

func validateAdmissionQuiescence(quiesced, createOnly bool) error {
	if !quiesced && !createOnly {
		return fmt.Errorf("storage-admit requires --writers-quiesced unless the object admission explicitly enables create-only publication")
	}
	return nil
}

// validateAdmissionAccounting cannot establish provider quiescence or inventory
// correctness. It binds the operator's explicit physical declaration to evidence;
// current native HEAD/list observations are checked separately without mutation.
func validateAdmissionAccounting(cfg *config.Config, proof json.RawMessage, physicalProvided bool, physical int64) (bool, error) {
	if cfg == nil || !cfg.Storage.CoordinatedWrites {
		return false, fmt.Errorf("storage-admit requires FILE_COORDINATED_WRITES=true")
	}
	switch cfg.BackendName() {
	case "local":
		if physicalProvided {
			return false, fmt.Errorf("Local storage-admit measures physical bytes from the verified native tree; --physical-bytes is only for COS/S3")
		}
		return false, nil
	case "cos", "s3":
		if !physicalProvided || physical < 0 || physical > 9007199254740991 {
			return true, fmt.Errorf("COS/S3 storage-admit requires explicit --physical-bytes including current objects and retained versions")
		}
		if err := validateObjectAdmissionEvidence(proof); err != nil {
			return true, err
		}
		return true, nil
	default:
		return false, fmt.Errorf("storage-admit supports local, cos or s3")
	}
}

func validateObjectAdmissionEvidence(proof json.RawMessage) error {
	var document map[string]json.RawMessage
	if json.Unmarshal(proof, &document) != nil || document == nil {
		return fmt.Errorf("object admission evidence must be a JSON object with retainedVersionInventory")
	}
	var inventory string
	if json.Unmarshal(document["retainedVersionInventory"], &inventory) != nil || strings.TrimSpace(inventory) == "" || len(inventory) > 4096 || strings.ContainsFunc(inventory, unicode.IsControl) {
		return fmt.Errorf("object admission evidence requires a nonempty retainedVersionInventory reference for the exact owner/root, including retained object generations")
	}
	return nil
}
func admissionPhysicalBytes(objects bool, logical, declared int64) (int64, error) {
	if logical < 0 || logical > 9007199254740991 {
		return 0, fmt.Errorf("storage-admit logical usage is outside the supported integer range")
	}
	if !objects {
		return logical, nil
	}
	if declared < logical || declared < 0 || declared > 9007199254740991 {
		return 0, fmt.Errorf("object --physical-bytes must cover all verified current native bytes plus retained versions")
	}
	return declared, nil
}
