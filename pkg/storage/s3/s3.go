// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package s3 implements storage.Storage on top of an S3-compatible object
// store. It uses the AWS SDK for Go v2 and works against Amazon S3 as well as
// MinIO and other S3-compatible endpoints (via a custom endpoint and
// path-style addressing).
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/durupages/durupages/pkg/storage"
)

// Options configures a Client.
type Options struct {
	// Endpoint is a custom S3 endpoint URL (e.g. "http://localhost:9000" for a
	// local MinIO). Leave empty to use the default AWS endpoint for Region.
	Endpoint string
	// Region is the S3 region. It is required by the SDK even for MinIO; use a
	// placeholder such as "us-east-1" when talking to MinIO.
	Region string
	// Bucket is the bucket all objects are stored in. Required.
	Bucket string
	// AccessKey and SecretKey are static credentials. When AccessKey is empty
	// the SDK default credential chain is used (environment, shared config,
	// IAM role, etc.).
	AccessKey string
	SecretKey string
	// UsePathStyle forces path-style addressing (bucket in the URL path rather
	// than the host). Required for most MinIO deployments.
	UsePathStyle bool
}

// Client is an S3-backed storage.Storage.
type Client struct {
	api    *awss3.Client
	bucket string
}

// compile-time check that *Client implements storage.Storage.
var _ storage.Storage = (*Client)(nil)

// New builds a Client from opts. When opts.AccessKey is set, static
// credentials are used; otherwise the SDK default credential chain is loaded.
// A custom Endpoint and UsePathStyle enable MinIO compatibility.
func New(ctx context.Context, opts Options) (*Client, error) {
	if opts.Bucket == "" {
		return nil, errors.New("s3: Bucket is required")
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if opts.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(opts.Region))
	}
	if opts.AccessKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, ""),
		))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3: load config: %w", err)
	}

	api := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
		o.UsePathStyle = opts.UsePathStyle
	})

	return &Client{api: api, bucket: opts.Bucket}, nil
}

// Get streams the object at key. The returned reader is the raw response body;
// it is not buffered in memory and must be closed by the caller. A missing key
// is reported as storage.ErrNotFound.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	out, err := c.api.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, storage.ObjectInfo{}, mapError(err)
	}

	info := storage.ObjectInfo{
		Key:  key,
		Size: aws.ToInt64(out.ContentLength),
		ETag: normalizeETag(aws.ToString(out.ETag)),
	}
	if out.ContentType != nil {
		info.ContentType = *out.ContentType
	}
	return out.Body, info, nil
}

// Put stores r at key. size is the exact content length, or -1 if unknown; a
// known size lets the SDK send it directly without buffering.
func (c *Client) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	in := &awss3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Body:   r,
	}
	if size >= 0 {
		in.ContentLength = aws.Int64(size)
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	if _, err := c.api.PutObject(ctx, in); err != nil {
		return mapError(err)
	}
	return nil
}

// Delete removes key. Deleting a missing key is not an error (S3 DeleteObject
// is idempotent).
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.api.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return mapError(err)
	}
	return nil
}

// List returns the metadata of every object whose key starts with prefix,
// paging through the full result set with ListObjectsV2. The returned slice is
// non-nil and ordered by key (S3 returns keys in lexicographic order).
func (c *Client) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	in := &awss3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
	}
	if prefix != "" {
		in.Prefix = aws.String(prefix)
	}

	infos := make([]storage.ObjectInfo, 0)
	paginator := awss3.NewListObjectsV2Paginator(c.api, in)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, obj := range page.Contents {
			infos = append(infos, storage.ObjectInfo{
				Key:  aws.ToString(obj.Key),
				Size: aws.ToInt64(obj.Size),
				ETag: normalizeETag(aws.ToString(obj.ETag)),
			})
		}
	}
	return infos, nil
}

// normalizeETag strips the surrounding double quotes S3 wraps ETags in.
func normalizeETag(etag string) string {
	if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
		return etag[1 : len(etag)-1]
	}
	return etag
}

// mapError translates S3 "no such key"/"not found" errors into
// storage.ErrNotFound and passes everything else through unchanged.
func mapError(err error) error {
	if err == nil {
		return nil
	}

	var noSuchKey *s3types.NoSuchKey
	var notFound *s3types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return storage.ErrNotFound
	}

	// MinIO and some proxies do not always deserialize into the typed shapes
	// above; fall back to the wire error code / HTTP status.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return storage.ErrNotFound
		}
	}
	return err
}
