package api

import (
	"net/url"
	"strings"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storage"
)

const (
	filesReadGrantWindow = time.Hour
	filesTransferPrefix  = "/api/files/transfer/"
)

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

// filesPartGrantPayload writes one class A grant. The provider URL is exposed
// only through this single-session grant; the optional edge relay rewrites it
// to the reserved same-origin prefix without decoding the native key.
func (s *Server) filesPartGrantPayload(grant storage.SignedPartGrant) (*filesPartGrant, error) {
	value := grant.URL
	if len(grant.Form) > 0 && s.cfg != nil && s.cfg.EdgeOneUploadIngress() {
		// The edge ingress rewrites a provider URL to a same-origin path that serves PUT
		// only; a form upload carries every signed value in its body and cannot be rewritten.
		return nil, fc.NewProblem(503, "temporarily_unavailable", "A form upload cannot use the edge upload ingress.")
	}
	if s.cfg != nil && s.cfg.EdgeOneUploadIngress() {
		private, err := url.Parse(grant.URL)
		if err != nil || private.Scheme != "https" && private.Scheme != "http" || private.Host == "" || private.User != nil || private.Fragment != "" || !strings.HasPrefix(private.EscapedPath(), "/") {
			return nil, fc.NewProblem(503, "temporarily_unavailable", "The provider part grant is invalid.")
		}
		value = filesTransferPrefix + strings.TrimPrefix(private.EscapedPath(), "/")
		if private.RawQuery != "" {
			value += "?" + private.RawQuery
		}
	}
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
