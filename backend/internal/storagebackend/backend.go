// Package storagebackend wires operator configuration to native owner trees.
// Legacy COS routes remain separate until the unified HTTP API is implemented.
package storagebackend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/cos"
	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/s3storage"
	"everythingabout.me/backend/internal/storage"
	"golang.org/x/text/unicode/norm"
)

type Backend struct {
	cfg           config.Config
	factory       storage.Factory
	legacy        storage.Storage
	close         func() error
	mu            sync.RWMutex
	closed        bool
	objectBackend objectmutation.Backend
	objectOptions objectmutation.Options
	// objectProblem explains why coordinated object writes are unavailable. It is
	// only set by NewForServing, which keeps serving when measured capability is
	// missing instead of refusing to start.
	objectProblem error
}

// New constructs a backend that refuses to run with unproven object-write
// capability. Maintenance commands use it so a partially working process can
// never be mistaken for an admitted one.
func New(cfg *config.Config) (*Backend, error) { return newBackend(cfg, false) }

// NewForServing is New for the long-running server: a missing or stale admission
// record disables coordinated object writes and is reported, but the process still
// serves reads. Storage decides what is true; the admission record only decides
// whether this process may publish.
func NewForServing(cfg *config.Config) (*Backend, error) { return newBackend(cfg, true) }

// ObjectWritesProblem reports why coordinated object writes are disabled. Nil
// means the configured connection is admitted.
func (b *Backend) ObjectWritesProblem() error {
	if b == nil {
		return nil
	}
	return b.objectProblem
}

func newBackend(cfg *config.Config, tolerateUnprovenWrites bool) (*Backend, error) {
	if cfg == nil {
		return nil, fmt.Errorf("storage configuration is required")
	}
	if err := cfg.ValidateStorage(); err != nil {
		return nil, err
	}
	b := &Backend{cfg: *cfg}
	// Defaults support configuration assembled directly by maintenance/test callers.
	if b.cfg.Storage.ConnectionID == "" {
		b.cfg.Storage.ConnectionID = "default"
	}
	if b.cfg.Storage.Generation == 0 {
		b.cfg.Storage.Generation = 1
	}
	if b.cfg.Storage.S3.ContentPrefix == "" {
		b.cfg.Storage.S3.ContentPrefix = "drive/"
	}
	if b.cfg.Storage.S3.SystemPrefix == "" {
		b.cfg.Storage.S3.SystemPrefix = "__eam_system__/"
	}
	var err error
	switch cfg.BackendName() {
	case "local":
		var factory *localstorage.Factory
		factory, err = localstorage.NewFactory(localstorage.Options{CaseSensitive: cfg.Storage.Local.CaseSensitive})
		if err == nil {
			var check storage.Driver
			check, err = factory.Open(context.Background(), storage.Config{OwnerID: "configuration-check", ConnectionID: b.cfg.Storage.ConnectionID, Generation: b.cfg.Storage.Generation, ContentRoot: cfg.Storage.Local.ContentRoot, SystemRoot: cfg.Storage.Local.SystemRoot})
			if err == nil {
				err = check.Close()
			}
			b.factory = factory
		}
	case "cos":
		var client *cos.Client
		client, err = cos.New(cfg.COS)
		if err == nil {
			b.factory, err = client.TreeFactory()
			if b.cfg.Storage.CoordinatedWrites {
				b.legacy = &readOnlyLegacy{backend: b}
			} else {
				b.legacy = client
			}
			b.close = client.Close
			if err == nil && b.cfg.Storage.CoordinatedWrites {
				b.objectBackend, err = client.MutationBackend()
			}
		}
	case "s3":
		if err = validateObjectPrefixes(b.cfg.Storage.S3.ContentPrefix, b.cfg.Storage.S3.SystemPrefix); err != nil {
			return nil, err
		}
		opts := cfg.Storage.S3
		var factory *s3storage.Factory
		factory, err = s3storage.NewFactory(s3storage.Options{Endpoint: opts.Endpoint, Region: opts.Region, Bucket: opts.Bucket, PathStyle: opts.PathStyle, AccessKeyID: opts.AccessKeyID, SecretAccessKey: opts.SecretAccessKey, SessionToken: opts.SessionToken})
		b.factory = factory
		if err == nil && b.cfg.Storage.CoordinatedWrites {
			b.objectBackend, err = factory.MutationBackend()
		}
	}
	if err != nil {
		return nil, err
	}
	if b.objectBackend != nil {
		admission, problem := b.loadAdmission()
		switch {
		case problem != nil && !tolerateUnprovenWrites:
			return nil, problem
		case problem != nil:
			b.objectProblem = problem
			b.objectBackend = nil
		default:
			if !filepath.IsAbs(b.cfg.Storage.ObjectSpoolRoot) {
				return nil, fmt.Errorf("object writes require an absolute FILE_OBJECT_SPOOL_ROOT, got %q: %w", b.cfg.Storage.ObjectSpoolRoot, storage.NewError("invalid_config", "", nil))
			}
			max := b.cfg.Storage.MaxSinglePutBytes
			if max == 0 {
				max = 32 << 20
			}
			if max > 5<<30 {
				return nil, fmt.Errorf("object single-PUT publication exceeds the admitted 5 GiB limit")
			}
			b.objectOptions = objectmutation.Options{Admission: admission, SpoolRoot: b.cfg.Storage.ObjectSpoolRoot, MaxPublishBytes: max}
		}
	}
	if b.factory == nil {
		return nil, fmt.Errorf("unsupported storage backend")
	}
	if b.legacy == nil {
		b.legacy = &readOnlyLegacy{backend: b}
	}
	return b, nil
}

// loadAdmission resolves the measured evidence for this exact connection. Evidence
// is a measurement of one endpoint, so a bucket, region or endpoint change makes it
// inapplicable rather than merely old.
func (b *Backend) loadAdmission() (objectmutation.Admission, error) {
	admission, err := objectmutation.LoadAdmission(b.cfg.Storage.ObjectAdmissionFile)
	if err != nil {
		return admission, err
	}
	endpoint, bucket, region := b.cfg.Storage.S3.Endpoint, b.cfg.Storage.S3.Bucket, b.cfg.Storage.S3.Region
	if b.cfg.BackendName() == "cos" {
		bucket = b.cfg.COS.Bucket
		region = b.cfg.COS.Region
		endpoint = fmt.Sprintf("https://%s.cos.%s.myqcloud.com", bucket, region)
	}
	if !admission.Matches(b.cfg.Storage.ConnectionID, endpoint, bucket, region) {
		return admission, fmt.Errorf("object admission evidence %s was recorded for bucket %q in %q, but this connection is bucket %q in %q: %w",
			b.cfg.Storage.ObjectAdmissionFile, admission.Bucket, admission.Region, bucket, region, storage.NewError("invalid_config", "", nil))
	}
	if b.cfg.Storage.TransferClass == storage.TransferPresignedParts && !admission.MultipartUploads {
		return admission, fmt.Errorf("class A presigned-parts requires admitted multipartUploads evidence for this exact connection")
	}
	return admission, nil
}

func validateObjectPrefixes(content, system string) error {
	for _, prefix := range []string{content, system} {
		if !strings.HasSuffix(prefix, "/") || storage.ValidatePath(storage.Path(strings.TrimSuffix(prefix, "/"))) != nil || prefix == "/" {
			return storage.NewError("invalid_config", "", nil)
		}
	}
	a, b := norm.NFC.String(content), norm.NFC.String(system)
	if strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
		return storage.NewError("invalid_config", "", nil)
	}
	return nil
}

// Binding is safe to persist; it deliberately contains neither secrets nor signed
// URLs. Its connection identity includes actual endpoint/bucket/root configuration.
func (b *Backend) Binding(ownerID string) (storage.Config, error) {
	if err := storage.ValidatePath(storage.Path(ownerID)); err != nil || ownerID == "" || strings.Contains(ownerID, "/") || strings.TrimSpace(ownerID) != ownerID || ownerID == "." || ownerID == ".." {
		return storage.Config{}, storage.NewError("invalid_config", "", nil)
	}
	cfg := storage.Config{OwnerID: ownerID, Generation: b.cfg.Storage.Generation}
	switch b.cfg.BackendName() {
	case "local":
		cfg.ContentRoot = filepath.Join(b.cfg.Storage.Local.ContentRoot, ownerID)
		cfg.SystemRoot = filepath.Join(b.cfg.Storage.Local.SystemRoot, ownerID)
	case "cos":
		cfg.ContentRoot = "drive/" + ownerID + "/"
		cfg.SystemRoot = "__eam_system__/" + ownerID + "/"
	case "s3":
		for _, prefix := range []string{b.cfg.Storage.S3.ContentPrefix, b.cfg.Storage.S3.SystemPrefix} {
			if !strings.HasSuffix(prefix, "/") || storage.ValidatePath(storage.Path(strings.TrimSuffix(prefix, "/"))) != nil {
				return storage.Config{}, storage.NewError("invalid_config", "", nil)
			}
		}
		cfg.ContentRoot = b.cfg.Storage.S3.ContentPrefix + ownerID + "/"
		cfg.SystemRoot = b.cfg.Storage.S3.SystemPrefix + ownerID + "/"
	}
	// Credential rotation never changes this digest. Any endpoint/region/root change does.
	identity := struct {
		Name, Backend, Bucket, Region, Endpoint, Content, System string
		PathStyle, CaseSensitive                                 bool
	}{Name: b.cfg.Storage.ConnectionID, Backend: b.cfg.BackendName(), Content: cfg.ContentRoot, System: cfg.SystemRoot}
	if b.cfg.BackendName() == "cos" {
		identity.Bucket = b.cfg.COS.Bucket
		identity.Region = b.cfg.COS.Region
	}
	if b.cfg.BackendName() == "s3" {
		identity.Bucket = b.cfg.Storage.S3.Bucket
		identity.Region = b.cfg.Storage.S3.Region
		identity.Endpoint = b.cfg.Storage.S3.Endpoint
		identity.PathStyle = b.cfg.Storage.S3.PathStyle
	}
	if b.cfg.BackendName() == "local" {
		identity.CaseSensitive = b.cfg.Storage.Local.CaseSensitive
	}
	data, _ := json.Marshal(identity)
	digest := sha256.Sum256(data)
	cfg.ConnectionID = b.cfg.Storage.ConnectionID + ":" + hex.EncodeToString(digest[:])
	if err := storage.ValidateBinding(cfg); err != nil {
		return storage.Config{}, err
	}
	return cfg, nil
}
func (b *Backend) Open(ctx context.Context, ownerID string) (storage.Driver, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return nil, storage.NewError("closed", "", nil)
	}
	cfg, err := b.Binding(ownerID)
	if err != nil {
		return nil, err
	}
	if factory, ok := b.factory.(*localstorage.Factory); ok {
		driver, err := factory.OpenOwner(ctx, cfg, b.cfg.Storage.Local.ContentRoot, b.cfg.Storage.Local.SystemRoot)
		if err != nil {
			return nil, err
		}
		if b.cfg.Storage.CoordinatedWrites {
			mutation, err := localstorage.NewMutationDriver(driver, localstorage.WriteOptions{CoordinatedExclusiveWriter: true})
			if err != nil {
				driver.Close()
				return nil, err
			}
			if err := b.checkTransferClass(mutation); err != nil {
				mutation.Close()
				return nil, err
			}
			return mutation, nil
		}
		if err := b.checkTransferClass(driver); err != nil {
			driver.Close()
			return nil, err
		}
		return driver, nil
	}
	driver, err := b.factory.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if b.objectBackend != nil {
		options := b.objectOptions
		options.Admission.ConnectionID = cfg.ConnectionID
		mutation, e := objectmutation.New(driver, cfg, b.objectBackend, options)
		if e != nil {
			driver.Close()
			return nil, e
		}
		if e := b.checkTransferClass(mutation); e != nil {
			mutation.Close()
			return nil, e
		}
		return mutation, nil
	}
	if err := b.checkTransferClass(driver); err != nil {
		driver.Close()
		return nil, err
	}
	return driver, nil
}

// checkTransferClass rejects a configured class the actual admitted driver does
// not advertise. A class is a property of the connection, never selected per request.
func (b *Backend) checkTransferClass(driver storage.Driver) error {
	want := b.cfg.Storage.TransferClass
	if want == "" {
		return nil
	}
	for _, class := range driver.Describe().TransferClasses {
		if class == want {
			return nil
		}
	}
	return storage.NewError("unsupported", "", nil)
}

// Legacy provides only reads for new backends. It never adds presigning/STS or
// unsafe native mutation emulation to satisfy the old interface.
func (b *Backend) Legacy() storage.Storage { return b.legacy }
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	if b.close != nil {
		return b.close()
	}
	return nil
}
