// Copyright (C) 2018 Storj Labs, Inc.
// See LICENSE for copying information.

package uplink

import (
	"context"
	"errors"

	"storj.io/storj/pkg/storj"
)

// A Macaroon represents an access credential to certain resources
type Macaroon interface {
	Serialize() ([]byte, error)
	Restrict(caveats ...Caveat) Macaroon
}

// Permissions are parsed by Uplink and return an Access struct
type Permissions struct {
	Macaroon Macaroon
}

// Caveat could be a read-only restriction, a time-bound
// restriction, a bucket-specific restriction, a path-prefix restriction, a
// full path restriction, etc.
type Caveat interface {
}

// ParseAccess parses a serialized Access
func ParseAccess(data []byte) (Access, error) {
	return Access{}, errors.New("not implemented")
}

// Serialize serializes an Access message
func (a *Access) Serialize() ([]byte, error) {
	return []byte{}, errors.New("not implemented")
}

// CreateBucket creates a bucket from the passed opts
func (a *Access) CreateBucket(ctx context.Context, bucket string, opts CreateBucketOptions) (storj.Bucket, error) {
	panic("TODO")
}

// DeleteBucket deletes a bucket if authorized
func (a *Access) DeleteBucket(ctx context.Context, bucket string) error {
	panic("TODO")
}

// ListBuckets will list authorized buckets
func (a *Access) ListBuckets(ctx context.Context, opts storj.BucketListOptions) (storj.BucketList, error) {
	panic("TODO")
}

// GetBucketInfo returns info about the requested bucket if authorized
func (a *Access) GetBucketInfo(ctx context.Context, bucket string) (storj.Bucket, error) {
	panic("TODO")
}

// GetBucket returns a Bucket with the given Encryption information
func (a *Access) GetBucket(ctx context.Context, bucket string, encryption Encryption) (*Bucket, error) {
	panic("TODO")
}