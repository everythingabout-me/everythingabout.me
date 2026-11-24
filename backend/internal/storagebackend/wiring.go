package storagebackend

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/cos"
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/s3storage"
)

// probeRoot is the only prefix the speed test may touch. It is reserved, never inside an
// owner content root and never returned to a client.
const probeRoot = "__eam_storage_probe__/"

// connectionIdentity is the endpoint a configuration selects, for the messages that name
// what was tried. Nothing keys off it any more: a product's capabilities no longer depend
// on which bucket they are being used against.
func connectionIdentity(cfg *config.Config) (endpoint, bucket, region string) {
	if cfg.BackendName() == "s3" {
		return cfg.Storage.S3.Endpoint, cfg.Storage.S3.Bucket, cfg.Storage.S3.Region
	}
	bucket, region = cfg.COS.Bucket, cfg.COS.Region
	return fmt.Sprintf("https://%s.cos.%s.myqcloud.com", bucket, region), bucket, region
}

// objectMutationBackend builds the write backend of one object connection. It is used by
// the speed test, which has to measure the native path a backup uses rather than a
// separate API — so it deliberately bypasses nothing: the product's capabilities already
// say what that path may do.
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

func randomProbeSuffix() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
