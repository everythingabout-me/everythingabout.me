package api

import (
	"net/url"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storage"
)

const filesReadGrantWindow = time.Hour

func (s *Server) filesTransferClass() string {
	if s.cfg == nil || s.cfg.Storage.TransferClass == "" {
		return storage.TransferRelay
	}
	return s.cfg.Storage.TransferClass
}

// filesTransferClassLabel is the short class letter used by every session and
// capability payload; filesTransferKind is the wire transport kind.
func (s *Server) filesTransferClassLabel() string {
	switch s.filesTransferClass() {
	case storage.TransferPresignedParts:
		return "A"
	case storage.TransferResumableSession:
		return "B"
	default:
		return "C"
	}
}

func (s *Server) filesTransferKind() string { return s.filesTransferClass() }

func filesUnderlyingDriver(driver storage.Driver) storage.Driver {
	switch d := driver.(type) {
	case *pendingDeleteDriver:
		return d.Driver
	case *pendingDeleteReferenceDriver:
		return d.pendingDeleteDriver.Driver
	default:
		return driver
	}
}

// filesPartGrantPayload writes one class A grant. The provider URL is exposed only
// through this single-session grant, and it is the provider's own URL: bytes go from the
// client to the store, never through this service.
func (s *Server) filesPartGrantPayload(grant storage.SignedPartGrant) (*filesPartGrant, error) {
	value := grant.URL
	headers := grant.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	payload := &filesPartGrant{Number: grant.Number, Size: grant.Size, Method: grant.Method, URL: value, Headers: headers, ExpiresAt: grant.ExpiresAt.UTC()}
	if len(grant.Form) > 0 {
		// A form upload carries every signed value in the body, so the edge relay cannot
		// rewrite it and the browser must not attach any header: the fields are the grant.
		fields := make([]filesFormField, 0, len(grant.Form))
		for _, field := range grant.Form {
			fields = append(fields, filesFormField{Name: field.Name, Value: field.Value})
		}
		payload.Fields = fields
		payload.FileField = grant.FileField
	}
	return payload, nil
}

func filesGrantExpiry(now time.Time) time.Time {
	// Round up to a fixed window boundary. Repeated grants inside one window
	// therefore return the identical provider URL and stay browser-cacheable.
	window := int64(filesReadGrantWindow / time.Second)
	return time.Unix((now.Unix()/window+1)*window, 0).UTC()
}

func filesContentSelectorURL(selector fc.EntrySelector, inline bool) string {
	values := url.Values{}
	if selector.IsID() {
		values.Set("id", selector.ID)
	} else {
		values.Set("path", string(selector.Path))
	}
	if inline {
		values.Set("disposition", "inline")
	} else {
		values.Set("disposition", "attachment")
	}
	return fc.Base + "/content?" + values.Encode()
}
