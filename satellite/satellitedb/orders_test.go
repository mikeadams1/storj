// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package satellitedb_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"storj.io/storj/internal/testcontext"
	"storj.io/storj/pkg/storj"
	"storj.io/storj/satellite"
	"storj.io/storj/satellite/orders"
	"storj.io/storj/satellite/satellitedb/satellitedbtest"
)

func TestSerialNumbers(t *testing.T) {
	satellitedbtest.Run(t, func(t *testing.T, db satellite.DB) {
		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		ordersDB := db.Orders()

		expectedBucket := []byte("bucketID")
		err := ordersDB.CreateSerialInfo(ctx, storj.SerialNumber{1}, expectedBucket, time.Now().UTC())
		require.NoError(t, err)

		bucketID, err := ordersDB.UseSerialNumber(ctx, storj.SerialNumber{1}, storj.NodeID{1})
		require.NoError(t, err)
		require.Equal(t, expectedBucket, bucketID)

		// try to use used serial number
		_, err = ordersDB.UseSerialNumber(ctx, storj.SerialNumber{1}, storj.NodeID{1})
		require.Error(t, err)
		require.True(t, orders.ErrUsingSerialNumber.Has(err))

		err = ordersDB.UnuseSerialNumber(ctx, storj.SerialNumber{1}, storj.NodeID{1})
		require.NoError(t, err)

		bucketID, err = ordersDB.UseSerialNumber(ctx, storj.SerialNumber{1}, storj.NodeID{1})
		require.NoError(t, err)
		require.Equal(t, expectedBucket, bucketID)

		// not existing serial number
		bucketID, err = ordersDB.UseSerialNumber(ctx, storj.SerialNumber{99}, storj.NodeID{1})
		require.Error(t, err)
		require.True(t, orders.ErrUsingSerialNumber.Has(err))
		require.Empty(t, bucketID)
	})
}
