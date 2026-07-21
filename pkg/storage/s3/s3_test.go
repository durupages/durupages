// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package s3_test

import (
	"context"
	"errors"
	"os"
	"testing"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/durupages/durupages/pkg/storage"
	s3 "github.com/durupages/durupages/pkg/storage/s3"
	"github.com/durupages/durupages/pkg/storage/storagetest"
)

// TestNewValidation checks constructor option handling without any network.
func TestNewValidation(t *testing.T) {
	if _, err := s3.New(context.Background(), s3.Options{Region: "us-east-1"}); err == nil {
		t.Fatal("New without Bucket: expected error, got nil")
	}
}

// TestNewStaticCredentials verifies a client is constructed with static
// credentials, a custom endpoint and path-style addressing (MinIO-style) with
// no outbound calls. New only builds config, so it must not touch the network.
func TestNewStaticCredentials(t *testing.T) {
	client, err := s3.New(context.Background(), s3.Options{
		Endpoint:     "http://localhost:9000",
		Region:       "us-east-1",
		Bucket:       "test-bucket",
		AccessKey:    "minioadmin",
		SecretKey:    "minioadmin",
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client == nil {
		t.Fatal("New returned nil client")
	}
}

// TestNewDefaultChain verifies construction with the default credential chain
// (no static keys).
func TestNewDefaultChain(t *testing.T) {
	client, err := s3.New(context.Background(), s3.Options{
		Region: "us-east-1",
		Bucket: "test-bucket",
	})
	if err != nil {
		t.Fatalf("New default chain: %v", err)
	}
	if client == nil {
		t.Fatal("New returned nil client")
	}
}

// genericAPIError is a minimal smithy.APIError for testing code-based mapping,
// as returned by MinIO-style endpoints that do not deserialize into the typed
// s3 error shapes.
type genericAPIError struct {
	code string
}

func (e *genericAPIError) Error() string                 { return e.code }
func (e *genericAPIError) ErrorCode() string             { return e.code }
func (e *genericAPIError) ErrorMessage() string          { return e.code }
func (e *genericAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

// TestMapError checks the translation of S3 errors into storage.ErrNotFound.
func TestMapError(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantNF  bool
		wantNil bool
	}{
		{name: "nil", err: nil, wantNil: true},
		{name: "typed NoSuchKey", err: &s3types.NoSuchKey{}, wantNF: true},
		{name: "typed NotFound", err: &s3types.NotFound{}, wantNF: true},
		{name: "code NoSuchKey", err: &genericAPIError{code: "NoSuchKey"}, wantNF: true},
		{name: "code NotFound", err: &genericAPIError{code: "NotFound"}, wantNF: true},
		{name: "code 404", err: &genericAPIError{code: "404"}, wantNF: true},
		{name: "wrapped NoSuchKey", err: errors.Join(errors.New("ctx"), &s3types.NoSuchKey{}), wantNF: true},
		{name: "other API error", err: &genericAPIError{code: "AccessDenied"}, wantNF: false},
		{name: "plain error", err: errors.New("boom"), wantNF: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s3.MapError(tc.err)
			switch {
			case tc.wantNil:
				if got != nil {
					t.Fatalf("MapError = %v, want nil", got)
				}
			case tc.wantNF:
				if !errors.Is(got, storage.ErrNotFound) {
					t.Fatalf("MapError = %v, want storage.ErrNotFound", got)
				}
			default:
				if errors.Is(got, storage.ErrNotFound) {
					t.Fatalf("MapError = %v, unexpectedly mapped to ErrNotFound", got)
				}
				if got == nil {
					t.Fatal("MapError = nil, want passthrough error")
				}
			}
		})
	}
}

// TestIntegration runs the full conformance suite against a live S3-compatible
// endpoint when DURUPAGES_TEST_S3_ENDPOINT (and the bucket/credentials) are
// set; otherwise it is skipped.
func TestIntegration(t *testing.T) {
	endpoint := os.Getenv("DURUPAGES_TEST_S3_ENDPOINT")
	bucket := os.Getenv("DURUPAGES_TEST_S3_BUCKET")
	accessKey := os.Getenv("DURUPAGES_TEST_S3_ACCESS_KEY")
	secretKey := os.Getenv("DURUPAGES_TEST_S3_SECRET_KEY")
	if endpoint == "" || bucket == "" {
		t.Skip("DURUPAGES_TEST_S3_ENDPOINT/BUCKET not set; skipping S3 integration tests")
	}

	region := os.Getenv("DURUPAGES_TEST_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}

	client, err := s3.New(context.Background(), s3.Options{
		Endpoint:     endpoint,
		Region:       region,
		Bucket:       bucket,
		AccessKey:    accessKey,
		SecretKey:    secretKey,
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	storagetest.RunConformance(t, func(t *testing.T) storage.Storage {
		// A single bucket is shared across subtests; use a unique key prefix so
		// leftover objects from other runs do not interfere. The conformance
		// suite uses fixed keys, so clean up what we create.
		t.Cleanup(func() {
			infos, err := client.List(context.Background(), "tenants/")
			if err != nil {
				return
			}
			for _, info := range infos {
				_ = client.Delete(context.Background(), info.Key)
			}
		})
		return client
	})
}
