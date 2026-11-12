package storagebackend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/cos"
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/s3storage"
	"everythingabout.me/backend/internal/storage"
)

// probeRoot is the only prefix these probes may touch. It is reserved, never
// inside an owner content root and never returned to a client.
const probeRoot = "__eam_storage_probe__/"

// probePartBytes is small enough to be cheap and large enough to be a legal
// single part for providers that require a minimum size.
const probePartBytes = 1 << 20

// connectionIdentity is the endpoint a configuration selects. Evidence describes
// exactly one of these; there is no "close enough".
func connectionIdentity(cfg *config.Config) (endpoint, bucket, region string) {
	if cfg.BackendName() == "s3" {
		return cfg.Storage.S3.Endpoint, cfg.Storage.S3.Bucket, cfg.Storage.S3.Region
	}
	bucket, region = cfg.COS.Bucket, cfg.COS.Region
	return fmt.Sprintf("https://%s.cos.%s.myqcloud.com", bucket, region), bucket, region
}

// EnsureAdmission refreshes object-write evidence for the configured connection.
//
// Evidence is a measurement, so it cannot be carried across a bucket, region or
// endpoint change: what was true on the old connection says nothing about the new
// one. Rather than make the operator re-run a test suite by hand, probe here and
// record the result. Configuration decides which tree to use; the probe only
// establishes that the write protocol is safe on it.
func EnsureAdmission(ctx context.Context, cfg *config.Config) error {
	if cfg == nil || !cfg.Storage.CoordinatedWrites {
		return nil
	}
	switch cfg.BackendName() {
	case "cos", "s3":
	default:
		return nil
	}
	path := cfg.Storage.ObjectAdmissionFile
	endpoint, bucket, region := connectionIdentity(cfg)
	previous, loadErr := objectmutation.LoadAdmission(path)
	if loadErr == nil && previous.Matches(cfg.Storage.ConnectionID, endpoint, bucket, region) {
		return nil
	}
	admission, err := probeConnectionEvidence(ctx, cfg)
	if err != nil {
		return err
	}
	slog.Info("refreshed object admission evidence for the configured connection",
		"path", path, "bucket", bucket, "region", region,
		"conditional_create", admission.ConditionalCreate,
		"atomic_publish", admission.AtomicPublish,
		"multipart_uploads", admission.MultipartUploads)
	return nil
}

// ProbeConnection measures one storage connection on demand and writes the evidence
// its driver requires before it may publish.
//
// It exists because the server can only probe the connection its own environment
// describes: a row added later (a second bucket, another endpoint) has no startup
// path that would measure it, so without this command that connection stays
// permanently "unproven" and every write to it is refused. `add` therefore inserts
// a row that can read but not publish until this command has run once.
func ProbeConnection(ctx context.Context, cfg *config.Config) (objectmutation.Admission, error) {
	if cfg == nil {
		return objectmutation.Admission{}, fmt.Errorf("storage configuration is required")
	}
	name := cfg.Storage.ConnectionID
	switch cfg.BackendName() {
	case "cos", "s3":
	default:
		return objectmutation.Admission{}, fmt.Errorf("storage connection %q uses backend %q: only cos and s3 publish through the coordinated object protocol",
			name, cfg.BackendName())
	}
	if !cfg.Storage.CoordinatedWrites {
		return objectmutation.Admission{}, fmt.Errorf("storage connection %q has coordinated writes disabled, so it has no publication path to probe: add it with --coordinated-writes=true",
			name)
	}
	if !filepath.IsAbs(cfg.Storage.ObjectAdmissionFile) {
		return objectmutation.Admission{}, fmt.Errorf("storage connection %q has no absolute admission evidence path, so the probe result could not be recorded where its driver reads it",
			name)
	}
	return probeConnectionEvidence(ctx, cfg)
}

// probeConnectionEvidence measures one object connection and records the answer.
//
// The measurement is written before it is judged: a provider that fails a property
// still leaves the transcript naming what was tried, which is the difference between
// "this endpoint cannot publish" and an unexplained refusal.
func probeConnectionEvidence(ctx context.Context, cfg *config.Config) (objectmutation.Admission, error) {
	path := cfg.Storage.ObjectAdmissionFile
	endpoint, bucket, region := connectionIdentity(cfg)
	previous, loadErr := objectmutation.LoadAdmission(path)
	backend, closer, err := objectMutationBackend(cfg)
	if err != nil {
		return objectmutation.Admission{}, fmt.Errorf("storage connection %q: open the provider for probing: %w", cfg.Storage.ConnectionID, err)
	}
	if closer != nil {
		defer closer.Close()
	}
	admission, observations, err := ProbeAdmission(ctx, backend)
	if err != nil {
		return admission, fmt.Errorf("storage connection %q: probe %s bucket %q region %q: %w",
			cfg.Storage.ConnectionID, endpoint, bucket, region, err)
	}
	admission.ConnectionID = cfg.Storage.ConnectionID
	admission.Endpoint = endpoint
	admission.Bucket = bucket
	admission.Region = region
	// singleGateway is a deployment property (this application is the only
	// publisher), not something a request can measure. Carry the operator's
	// previous assertion instead of inventing one.
	admission.SingleGateway = loadErr != nil || previous.SingleGateway
	// These two are not measured by this probe, so dropping them would silently retire a
	// measured optimization on the next start. They describe the same connection — a probe
	// version bump or a re-measure changes nothing about the endpoint — so they carry over
	// only when the identity fields still match.
	if previous.Matches(cfg.Storage.ConnectionID, endpoint, bucket, region) {
		admission.ListingETag = previous.ListingETag
		admission.SignedMultipartParts = previous.SignedMultipartParts
	}
	record := filepath.Join(filepath.Dir(path), filepath.Base(path)+".probe.json")
	if err := writeJSONFile(record, observations); err != nil {
		return admission, fmt.Errorf("storage connection %q: record the probe transcript: %w", cfg.Storage.ConnectionID, err)
	}
	admission.ProbeReference = record
	if err := objectmutation.ValidateAdmission(admission); err != nil {
		return admission, fmt.Errorf("storage connection %q at %s bucket %q cannot be admitted for object writes (transcript: %s): %w",
			cfg.Storage.ConnectionID, endpoint, bucket, record, err)
	}
	if err := writeJSONFile(path, admission); err != nil {
		return admission, fmt.Errorf("storage connection %q: write admission evidence %s: %w", cfg.Storage.ConnectionID, path, err)
	}
	return admission, nil
}

// objectMutationBackend builds the write backend of one object connection without
// consulting admission evidence. Only the probe may call this: a backend built this
// way has no verified write protocol and must never serve a request.
func objectMutationBackend(cfg *config.Config) (objectmutation.Backend, io.Closer, error) {
	switch cfg.BackendName() {
	case "cos":
		client, err := cos.New(cfg.COS)
		if err != nil {
			return nil, nil, err
		}
		backend, err := client.MutationBackend()
		if err != nil {
			client.Close()
			return nil, nil, err
		}
		return backend, client, nil
	case "s3":
		factory, err := s3storage.NewFactory(s3storage.Options{
			Endpoint: cfg.Storage.S3.Endpoint, Region: cfg.Storage.S3.Region, Bucket: cfg.Storage.S3.Bucket,
			PathStyle: cfg.Storage.S3.PathStyle, AccessKeyID: cfg.Storage.S3.AccessKeyID,
			SecretAccessKey: cfg.Storage.S3.SecretAccessKey, SessionToken: cfg.Storage.S3.SessionToken,
		})
		if err != nil {
			return nil, nil, err
		}
		backend, err := factory.MutationBackend()
		if err != nil {
			return nil, nil, err
		}
		return backend, nil, nil
	}
	return nil, nil, fmt.Errorf("backend %q has no object mutation backend", cfg.BackendName())
}

// writeJSONFile replaces a file atomically so a crash never leaves half evidence.
func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".admission-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// probeObservation is the operator-auditable transcript of one measured property.
// It is written next to the evidence so a later reader can see what was actually
// tried instead of trusting a boolean.
type probeObservation struct {
	Property string `json:"property"`
	Request  string `json:"request"`
	Result   string `json:"result"`
	Bytes    string `json:"bytes"`
}

// ProbeAdmission measures the capabilities coordinated writes depend on, using
// only the reserved probe prefix. It never asserts a capability it did not
// observe: a missing or failed property leaves the corresponding flag false.
func ProbeAdmission(ctx context.Context, backend objectmutation.Backend) (objectmutation.Admission, []probeObservation, error) {
	multipart, _ := backend.(objectmutation.MultipartBackend)
	suffix, err := randomProbeSuffix()
	if err != nil {
		return objectmutation.Admission{}, nil, err
	}
	root := probeRoot + suffix + "/"
	createKey, multipartKey := root+"create-only", root+"multipart"
	observations := []probeObservation{}
	admission := objectmutation.Admission{CheckedAt: time.Now().UTC(), Version: objectmutation.AdmissionVersion}

	created, err := backend.Put(ctx, objectmutation.Put{Key: createKey, Body: strings.NewReader("probe-original"), Size: int64(len("probe-original")), MediaType: "application/octet-stream", Absent: true})
	if err != nil {
		return admission, observations, fmt.Errorf("object admission probe: create %s: %w", createKey, err)
	}
	observations = append(observations, probeObservation{Property: "conditionalCreate", Request: "PUT absent=true", Result: "created", Bytes: "probe-original"})
	defer func() { cleanupProbe(context.Background(), backend, createKey) }()

	_, err = backend.Put(ctx, objectmutation.Put{Key: createKey, Body: strings.NewReader("probe-replacement"), Size: int64(len("probe-replacement")), MediaType: "application/octet-stream", Absent: true})
	switch {
	case storage.IsCode(err, "precondition_failed"):
		observations = append(observations, probeObservation{Property: "conditionalCreate", Request: "PUT absent=true on an occupied key", Result: "rejected", Bytes: "probe-original"})
		admission.ConditionalCreate = true
	case err == nil:
		observations = append(observations, probeObservation{Property: "conditionalCreate", Request: "PUT absent=true on an occupied key", Result: "accepted (provider replaced the object)", Bytes: "probe-replacement"})
	default:
		return admission, observations, fmt.Errorf("object admission probe: conditional create on %s: %w", createKey, err)
	}

	// Read the object back through If-Match on the version we created. Conditional
	// create is only useful if the original bytes are still the ones served.
	if err := probeBytesIntact(ctx, backend, createKey, created.ETag, "probe-original"); err != nil {
		observations = append(observations, probeObservation{Property: "atomicPublish", Request: "GET If-Match <created ETag>", Result: "drifted", Bytes: err.Error()})
	} else {
		observations = append(observations, probeObservation{Property: "atomicPublish", Request: "GET If-Match <created ETag>", Result: "unchanged", Bytes: "probe-original"})
		admission.AtomicPublish = admission.ConditionalCreate
	}

	// The overwrite switch on its own, with no conditional header: that is the whole
	// guarantee a form upload has, because a form cannot send If-None-Match. Asked last so
	// a provider that ignores it (and replaces the object) cannot spoil the checks above.
	_, err = backend.Put(ctx, objectmutation.Put{Key: createKey, Body: strings.NewReader("probe-forbid"), Size: int64(len("probe-forbid")), MediaType: "application/octet-stream", ForbidOverwrite: true})
	switch {
	case storage.IsCode(err, "precondition_failed"):
		observations = append(observations, probeObservation{Property: "formPost", Request: "PUT forbid-overwrite on an occupied key", Result: "rejected", Bytes: "probe-original"})
		admission.FormPost = true
	case err == nil:
		observations = append(observations, probeObservation{Property: "formPost", Request: "PUT forbid-overwrite on an occupied key", Result: "accepted (provider replaced the object)", Bytes: "probe-forbid"})
	case storage.IsCode(err, "unsupported"):
		observations = append(observations, probeObservation{Property: "formPost", Request: "PUT forbid-overwrite on an occupied key", Result: "unsupported"})
	default:
		return admission, observations, fmt.Errorf("object admission probe: overwrite switch on %s: %w", createKey, err)
	}

	if multipart != nil {
		admission.MultipartUploads = probeMultipart(ctx, backend, multipart, multipartKey, &observations)
	}
	return admission, observations, nil
}

// probeMultipart proves part persistence and conditional create on Complete,
// which is a different code path from a single PUT on COS.
func probeMultipart(ctx context.Context, whole objectmutation.Backend, backend objectmutation.MultipartBackend, key string, observations *[]probeObservation) bool {
	body := strings.Repeat("p", probePartBytes)
	complete := func(payload string) (objectmutation.Object, string, error) {
		id, err := backend.BeginMultipart(ctx, objectmutation.Put{Key: key, MediaType: "application/octet-stream", Size: int64(len(payload)), Absent: true})
		if err != nil {
			return objectmutation.Object{}, "", err
		}
		etag, err := backend.PutPart(ctx, key, id, 1, strings.NewReader(payload), int64(len(payload)))
		if err != nil {
			_ = backend.AbortMultipart(context.Background(), key, id)
			return objectmutation.Object{}, id, err
		}
		object, err := backend.CompleteMultipart(ctx, objectmutation.Put{Key: key, MediaType: "application/octet-stream", Size: int64(len(payload)), Absent: true}, id, []objectmutation.NativePart{{Number: 1, ETag: etag}})
		return object, id, err
	}
	first, _, err := complete(body)
	if err != nil {
		*observations = append(*observations, probeObservation{Property: "multipartUploads", Request: "Begin/UploadPart/Complete absent=true", Result: "failed: " + err.Error()})
		return false
	}
	defer func() { cleanupProbe(context.Background(), whole, key) }()
	*observations = append(*observations, probeObservation{Property: "multipartUploads", Request: "Begin/UploadPart/Complete absent=true", Result: "created", Bytes: "1 MiB part"})

	replacement := strings.Repeat("r", probePartBytes)
	_, secondID, err := complete(replacement)
	switch {
	case storage.IsCode(err, "precondition_failed"):
		*observations = append(*observations, probeObservation{Property: "multipartUploads", Request: "Complete absent=true on an occupied key", Result: "rejected", Bytes: "1 MiB part"})
		if err := probeBytesIntact(ctx, whole, key, first.ETag, body); err != nil {
			*observations = append(*observations, probeObservation{Property: "multipartUploads", Request: "GET If-Match <completed ETag>", Result: "drifted: " + err.Error()})
			return false
		}
		*observations = append(*observations, probeObservation{Property: "multipartUploads", Request: "GET If-Match <completed ETag>", Result: "unchanged", Bytes: "1 MiB part"})
		return true
	case err == nil:
		*observations = append(*observations, probeObservation{Property: "multipartUploads", Request: "Complete absent=true on an occupied key", Result: "accepted (provider replaced the object)"})
		return false
	default:
		if secondID != "" {
			_ = backend.AbortMultipart(context.Background(), key, secondID)
		}
		*observations = append(*observations, probeObservation{Property: "multipartUploads", Request: "Complete absent=true on an occupied key", Result: "error: " + err.Error()})
		return false
	}
}

func probeBytesIntact(ctx context.Context, backend objectmutation.Backend, key, etag, want string) error {
	if etag == "" {
		return fmt.Errorf("no ETag recorded")
	}
	reader, err := backend.Open(ctx, key, etag)
	if err != nil {
		return err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, int64(len(want))+1))
	if err != nil {
		return err
	}
	if string(data) != want {
		return fmt.Errorf("bytes differ")
	}
	return nil
}

func cleanupProbe(ctx context.Context, backend objectmutation.Backend, key string) {
	object, err := backend.Head(ctx, key)
	if err != nil {
		return
	}
	_, _ = backend.Delete(ctx, key, object.ETag)
}

func randomProbeSuffix() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
