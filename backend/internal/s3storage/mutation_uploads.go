package s3storage

import (
	"context"
	"io"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var _ objectmutation.MultipartBackend = (*mutationBackend)(nil)

func (b *mutationBackend) BeginMultipart(ctx context.Context, in objectmutation.Put) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := b.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(b.bucket), Key: aws.String(in.Key), ContentType: aws.String(in.MediaType), Metadata: cloneMutationMetadata(in.Metadata)})
	if err != nil {
		return "", mutationWriteError(in.Key, err)
	}
	if out == nil || aws.ToString(out.UploadId) == "" {
		return "", storage.NewError("invalid_backend_response", storage.Path(in.Key), nil)
	}
	return aws.ToString(out.UploadId), nil
}
func (b *mutationBackend) PutPart(ctx context.Context, key, id string, number int, body io.Reader, size int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := b.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: aws.String(id), PartNumber: aws.Int32(int32(number)), Body: body, ContentLength: aws.Int64(size)})
	if err != nil {
		return "", mutationWriteError(key, err)
	}
	if out == nil || aws.ToString(out.ETag) == "" {
		return "", storage.NewError("invalid_backend_response", storage.Path(key), nil)
	}
	return aws.ToString(out.ETag), nil
}
func (b *mutationBackend) CompleteMultipart(ctx context.Context, in objectmutation.Put, id string, parts []objectmutation.NativePart) (objectmutation.Object, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	manifest := &types.CompletedMultipartUpload{}
	for _, p := range parts {
		manifest.Parts = append(manifest.Parts, types.CompletedPart{PartNumber: aws.Int32(int32(p.Number)), ETag: aws.String(p.ETag)})
	}
	input := &s3.CompleteMultipartUploadInput{Bucket: aws.String(b.bucket), Key: aws.String(in.Key), UploadId: aws.String(id), MultipartUpload: manifest}
	if in.Absent {
		input.IfNoneMatch = aws.String("*")
	}
	if in.ExpectedETag != "" {
		input.IfMatch = aws.String(in.ExpectedETag)
	}
	out, err := b.client.CompleteMultipartUpload(ctx, input)
	if err != nil {
		return objectmutation.Object{}, mutationWriteError(in.Key, err)
	}
	return objectmutation.Object{Key: in.Key, VersionID: aws.ToString(out.VersionId), ETag: aws.ToString(out.ETag), Size: in.Size, MediaType: in.MediaType, Metadata: cloneMutationMetadata(in.Metadata)}, nil
}
func (b *mutationBackend) AbortMultipart(ctx context.Context, key, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, err := b.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(b.bucket), Key: aws.String(key), UploadId: aws.String(id)})
	if err != nil {
		mapped := mutationReadError(ctx, key, err, false)
		if storage.IsCode(mapped, "not_found") {
			return nil
		}
		return mapped
	}
	return nil
}
