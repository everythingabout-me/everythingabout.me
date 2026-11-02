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
	"everythingabout.me/backend/internal/s3storage"
	"everythingabout.me/backend/internal/storage"
	"golang.org/x/text/unicode/norm"
)

type Backend struct {
	cfg     config.Config
	factory storage.Factory
	legacy  storage.Storage
	close   func() error
	mu      sync.RWMutex
	closed  bool
}

func New(cfg *config.Config) (*Backend, error) {
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
			b.legacy = client
			b.close = client.Close
		}
	case "s3":
		if err = validateObjectPrefixes(b.cfg.Storage.S3.ContentPrefix, b.cfg.Storage.S3.SystemPrefix); err != nil {
			return nil, err
		}
		opts := cfg.Storage.S3
		b.factory, err = s3storage.NewFactory(s3storage.Options{Endpoint: opts.Endpoint, Region: opts.Region, Bucket: opts.Bucket, PathStyle: opts.PathStyle, AccessKeyID: opts.AccessKeyID, SecretAccessKey: opts.SecretAccessKey, SessionToken: opts.SessionToken})
	}
	if err != nil {
		return nil, err
	}
	if b.factory == nil {
		return nil, fmt.Errorf("unsupported storage backend")
	}
	if b.legacy == nil {
		b.legacy = &readOnlyLegacy{backend: b}
	}
	return b, nil
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
		if err != nil { return nil, err }
		if b.cfg.Storage.CoordinatedWrites {
			mutation, err := localstorage.NewMutationDriver(driver, localstorage.WriteOptions{CoordinatedExclusiveWriter:true})
			if err != nil { driver.Close(); return nil, err }
			return mutation,nil
		}
		return driver,nil
	}
	return b.factory.Open(ctx, cfg)
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
