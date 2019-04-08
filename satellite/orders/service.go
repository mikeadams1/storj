// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package orders

import (
	"context"
	"time"

	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/skyrings/skyring-common/tools/uuid"
	"github.com/zeebo/errs"
	"go.uber.org/zap"

	"storj.io/storj/pkg/auth/signing"
	"storj.io/storj/pkg/certdb"
	"storj.io/storj/pkg/eestream"
	"storj.io/storj/pkg/identity"
	"storj.io/storj/pkg/overlay"
	"storj.io/storj/pkg/pb"
	"storj.io/storj/pkg/storj"
)

// Service for creating order limits.
type Service struct {
	log       *zap.Logger
	satellite signing.Signer
	cache     *overlay.Cache
	certdb    certdb.DB
	orders    DB

	orderExpiration time.Duration
}

// NewService creates new service for creating order limits.
func NewService(log *zap.Logger, satellite signing.Signer, cache *overlay.Cache, certdb certdb.DB, orders DB, orderExpiration time.Duration) *Service {
	return &Service{
		log:             log,
		satellite:       satellite,
		cache:           cache,
		certdb:          certdb,
		orders:          orders,
		orderExpiration: orderExpiration,
	}
}

// VerifyOrderLimitSignature verifies that the signature inside order limit belongs to the satellite.
func (service *Service) VerifyOrderLimitSignature(signed *pb.OrderLimit2) error {
	return signing.VerifyOrderLimitSignature(service.satellite, signed)
}

func (service *Service) createSerial(ctx context.Context) (storj.SerialNumber, error) {
	uuid, err := uuid.New()
	if err != nil {
		return storj.SerialNumber{}, Error.Wrap(err)
	}
	return storj.SerialNumber(*uuid), nil
}

func (service *Service) saveSerial(ctx context.Context, serialNumber storj.SerialNumber, bucketID []byte, expiresAt time.Time) error {
	return service.orders.CreateSerialInfo(ctx, serialNumber, bucketID, expiresAt)
}

func (service *Service) updateBandwidth(ctx context.Context, bucketID []byte, addressedOrderLimits []*pb.AddressedOrderLimit) error {
	if len(addressedOrderLimits) == 0 {
		return nil
	}
	var bucketAllocation int64
	var action pb.PieceAction
	nodesAllocation := make(map[storj.NodeID]int64)
	for _, addressedOrderLimit := range addressedOrderLimits {
		if addressedOrderLimit != nil {
			orderLimit := addressedOrderLimit.Limit
			bucketAllocation += orderLimit.Limit
			nodesAllocation[orderLimit.StorageNodeId] = orderLimit.Limit
			action = orderLimit.Action
		}
	}
	now := time.Now().UTC()
	intervalStart := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())

	if err := service.orders.UpdateBucketBandwidthAllocation(ctx, bucketID, action, bucketAllocation, intervalStart); err != nil {
		return err
	}
	for nodeID, allocation := range nodesAllocation {
		if err := service.orders.UpdateStoragenodeBandwidthAllocation(ctx, nodeID, action, allocation, intervalStart); err != nil {
			return err
		}
	}
	return nil
}

// CreateGetOrderLimits creates the order limits for downloading the pieces of pointer.
func (service *Service) CreateGetOrderLimits(ctx context.Context, uplink *identity.PeerIdentity, bucketID []byte, pointer *pb.Pointer) (_ []*pb.AddressedOrderLimit, err error) {
	rootPieceID := pointer.GetRemote().RootPieceId
	expiration := pointer.ExpirationDate

	// convert orderExpiration from duration to timestamp
	orderExpirationTime := time.Now().UTC().Add(service.orderExpiration)
	orderExpiration, err := ptypes.TimestampProto(orderExpirationTime)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	serialNumber, err := service.createSerial(ctx)
	if err != nil {
		return nil, err
	}

	redundancy, err := eestream.NewRedundancyStrategyFromProto(pointer.GetRemote().GetRedundancy())
	if err != nil {
		return nil, Error.Wrap(err)
	}

	pieceSize := eestream.CalcPieceSize(pointer.GetSegmentSize(), redundancy)

	var combinedErrs error
	var limits []*pb.AddressedOrderLimit
	for _, piece := range pointer.GetRemote().GetRemotePieces() {
		node, err := service.cache.Get(ctx, piece.NodeId)
		if err != nil {
			service.log.Debug("error getting node from overlay cache", zap.Error(err))
			combinedErrs = errs.Combine(combinedErrs, err)
			continue
		}

		if node != nil {
			node.Type.DPanicOnInvalid("order service get order limits")
		}

		if !node.Online() {
			service.log.Debug("node is offline", zap.String("ID", node.Id.String()))
			combinedErrs = errs.Combine(combinedErrs, Error.New("node is offline: %s", node.Id.String()))
			continue
		}

		orderLimit, err := signing.SignOrderLimit(service.satellite, &pb.OrderLimit2{
			SerialNumber:    serialNumber,
			SatelliteId:     service.satellite.ID(),
			UplinkId:        uplink.ID,
			StorageNodeId:   piece.NodeId,
			PieceId:         rootPieceID.Derive(piece.NodeId),
			Action:          pb.PieceAction_GET,
			Limit:           pieceSize,
			PieceExpiration: expiration,
			OrderExpiration: orderExpiration,
		})
		if err != nil {
			return nil, Error.Wrap(err)
		}

		limits = append(limits, &pb.AddressedOrderLimit{
			Limit:              orderLimit,
			StorageNodeAddress: node.Address,
		})
	}

	if len(limits) < redundancy.RequiredCount() {
		err = Error.New("not enough nodes available: got %d, required %d", len(limits), redundancy.RequiredCount())
		return nil, errs.Combine(err, combinedErrs)
	}

	err = service.certdb.SavePublicKey(ctx, uplink.ID, uplink.Leaf.PublicKey)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	err = service.saveSerial(ctx, serialNumber, bucketID, orderExpirationTime)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	if err := service.updateBandwidth(ctx, bucketID, limits); err != nil {
		return nil, Error.Wrap(err)
	}

	return limits, nil
}

// CreatePutOrderLimits creates the order limits for uploading pieces to nodes.
func (service *Service) CreatePutOrderLimits(ctx context.Context, uplink *identity.PeerIdentity, bucketID []byte, nodes []*pb.Node, expiration *timestamp.Timestamp, maxPieceSize int64) (_ storj.PieceID, _ []*pb.AddressedOrderLimit, err error) {
	// convert orderExpiration from duration to timestamp
	orderExpirationTime := time.Now().UTC().Add(service.orderExpiration)
	orderExpiration, err := ptypes.TimestampProto(orderExpirationTime)
	if err != nil {
		return storj.PieceID{}, nil, Error.Wrap(err)
	}

	serialNumber, err := service.createSerial(ctx)
	if err != nil {
		return storj.PieceID{}, nil, err
	}

	rootPieceID := storj.NewPieceID()
	limits := make([]*pb.AddressedOrderLimit, len(nodes))
	var pieceNum int32
	for _, node := range nodes {
		orderLimit, err := signing.SignOrderLimit(service.satellite, &pb.OrderLimit2{
			SerialNumber:    serialNumber,
			SatelliteId:     service.satellite.ID(),
			UplinkId:        uplink.ID,
			StorageNodeId:   node.Id,
			PieceId:         rootPieceID.Derive(node.Id),
			Action:          pb.PieceAction_PUT,
			Limit:           maxPieceSize,
			PieceExpiration: expiration,
			OrderExpiration: orderExpiration,
		})
		if err != nil {
			return storj.PieceID{}, nil, Error.Wrap(err)
		}

		limits[pieceNum] = &pb.AddressedOrderLimit{
			Limit:              orderLimit,
			StorageNodeAddress: node.Address,
		}
		pieceNum++
	}

	err = service.certdb.SavePublicKey(ctx, uplink.ID, uplink.Leaf.PublicKey)
	if err != nil {
		return storj.PieceID{}, nil, Error.Wrap(err)
	}

	err = service.saveSerial(ctx, serialNumber, bucketID, orderExpirationTime)
	if err != nil {
		return storj.PieceID{}, nil, Error.Wrap(err)
	}

	if err := service.updateBandwidth(ctx, bucketID, limits); err != nil {
		return storj.PieceID{}, nil, Error.Wrap(err)
	}

	return rootPieceID, limits, nil
}

// CreateDeleteOrderLimits creates the order limits for deleting the pieces of pointer.
func (service *Service) CreateDeleteOrderLimits(ctx context.Context, uplink *identity.PeerIdentity, bucketID []byte, pointer *pb.Pointer) (_ []*pb.AddressedOrderLimit, err error) {
	rootPieceID := pointer.GetRemote().RootPieceId
	expiration := pointer.ExpirationDate

	// convert orderExpiration from duration to timestamp
	orderExpirationTime := time.Now().UTC().Add(service.orderExpiration)
	orderExpiration, err := ptypes.TimestampProto(orderExpirationTime)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	serialNumber, err := service.createSerial(ctx)
	if err != nil {
		return nil, err
	}

	var combinedErrs error
	var limits []*pb.AddressedOrderLimit
	for _, piece := range pointer.GetRemote().GetRemotePieces() {
		node, err := service.cache.Get(ctx, piece.NodeId)
		if err != nil {
			service.log.Debug("error getting node from overlay cache", zap.Error(err))
			combinedErrs = errs.Combine(combinedErrs, err)
			continue
		}

		if node != nil {
			node.Type.DPanicOnInvalid("order service delete order limits")
		}

		if !node.Online() {
			service.log.Debug("node is offline", zap.String("ID", node.Id.String()))
			combinedErrs = errs.Combine(combinedErrs, Error.New("node is offline: %s", node.Id.String()))
			continue
		}

		orderLimit, err := signing.SignOrderLimit(service.satellite, &pb.OrderLimit2{
			SerialNumber:    serialNumber,
			SatelliteId:     service.satellite.ID(),
			UplinkId:        uplink.ID,
			StorageNodeId:   piece.NodeId,
			PieceId:         rootPieceID.Derive(piece.NodeId),
			Action:          pb.PieceAction_DELETE,
			Limit:           0,
			PieceExpiration: expiration,
			OrderExpiration: orderExpiration,
		})
		if err != nil {
			return nil, Error.Wrap(err)
		}

		limits = append(limits, &pb.AddressedOrderLimit{
			Limit:              orderLimit,
			StorageNodeAddress: node.Address,
		})
	}

	if len(limits) == 0 {
		err = Error.New("failed creating order limits for all nodes")
		return nil, errs.Combine(err, combinedErrs)
	}

	err = service.certdb.SavePublicKey(ctx, uplink.ID, uplink.Leaf.PublicKey)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	err = service.saveSerial(ctx, serialNumber, bucketID, orderExpirationTime)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	return limits, nil
}

// CreateAuditOrderLimits creates the order limits for auditing the pieces of pointer.
func (service *Service) CreateAuditOrderLimits(ctx context.Context, auditor *identity.PeerIdentity, bucketID []byte, pointer *pb.Pointer) (_ []*pb.AddressedOrderLimit, err error) {
	rootPieceID := pointer.GetRemote().RootPieceId
	redundancy := pointer.GetRemote().GetRedundancy()
	shareSize := redundancy.GetErasureShareSize()
	totalPieces := redundancy.GetTotal()
	expiration := pointer.ExpirationDate

	// convert orderExpiration from duration to timestamp
	orderExpirationTime := time.Now().UTC().Add(service.orderExpiration)
	orderExpiration, err := ptypes.TimestampProto(orderExpirationTime)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	serialNumber, err := service.createSerial(ctx)
	if err != nil {
		return nil, err
	}

	var combinedErrs error
	var limitsCount int32
	limits := make([]*pb.AddressedOrderLimit, totalPieces)
	for _, piece := range pointer.GetRemote().GetRemotePieces() {
		node, err := service.cache.Get(ctx, piece.NodeId)
		if err != nil {
			service.log.Error("error getting node from the overlay cache", zap.Error(err))
			combinedErrs = errs.Combine(combinedErrs, err)
			continue
		}

		if node != nil {
			node.Type.DPanicOnInvalid("order service audit order limits")
		}

		if !node.Online() {
			service.log.Debug("node is offline", zap.String("ID", node.Id.String()))
			combinedErrs = errs.Combine(combinedErrs, Error.New("node is offline: %s", node.Id.String()))
			continue
		}

		orderLimit, err := signing.SignOrderLimit(service.satellite, &pb.OrderLimit2{
			SerialNumber:    serialNumber,
			SatelliteId:     service.satellite.ID(),
			UplinkId:        auditor.ID,
			StorageNodeId:   piece.NodeId,
			PieceId:         rootPieceID.Derive(piece.NodeId),
			Action:          pb.PieceAction_GET_AUDIT,
			Limit:           int64(shareSize),
			PieceExpiration: expiration,
			OrderExpiration: orderExpiration,
		})
		if err != nil {
			return nil, Error.Wrap(err)
		}

		limits[piece.GetPieceNum()] = &pb.AddressedOrderLimit{
			Limit:              orderLimit,
			StorageNodeAddress: node.Address,
		}
		limitsCount++
	}

	if limitsCount < redundancy.GetMinReq() {
		err = Error.New("not enough nodes available: got %d, required %d", limitsCount, redundancy.GetMinReq())
		return nil, errs.Combine(err, combinedErrs)
	}

	err = service.saveSerial(ctx, serialNumber, bucketID, orderExpirationTime)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	if err := service.updateBandwidth(ctx, bucketID, limits); err != nil {
		return nil, Error.Wrap(err)
	}

	return limits, nil
}

// CreateGetRepairOrderLimits creates the order limits for downloading the healthy pieces of pointer as the source for repair.
func (service *Service) CreateGetRepairOrderLimits(ctx context.Context, repairer *identity.PeerIdentity, bucketID []byte, pointer *pb.Pointer, healthy []*pb.RemotePiece) (_ []*pb.AddressedOrderLimit, err error) {
	rootPieceID := pointer.GetRemote().RootPieceId
	redundancy, err := eestream.NewRedundancyStrategyFromProto(pointer.GetRemote().GetRedundancy())
	if err != nil {
		return nil, Error.Wrap(err)
	}
	pieceSize := eestream.CalcPieceSize(pointer.GetSegmentSize(), redundancy)
	totalPieces := redundancy.TotalCount()
	expiration := pointer.ExpirationDate

	// convert orderExpiration from duration to timestamp
	orderExpirationTime := time.Now().UTC().Add(service.orderExpiration)
	orderExpiration, err := ptypes.TimestampProto(orderExpirationTime)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	serialNumber, err := service.createSerial(ctx)
	if err != nil {
		return nil, err
	}

	var combinedErrs error
	var limitsCount int
	limits := make([]*pb.AddressedOrderLimit, totalPieces)
	for _, piece := range healthy {
		node, err := service.cache.Get(ctx, piece.NodeId)
		if err != nil {
			service.log.Error("error getting node from the overlay cache", zap.Error(err))
			combinedErrs = errs.Combine(combinedErrs, err)
			continue
		}

		if node != nil {
			node.Type.DPanicOnInvalid("order service get repair order limits")
		}

		if !node.Online() {
			service.log.Debug("node is offline", zap.String("ID", node.Id.String()))
			combinedErrs = errs.Combine(combinedErrs, Error.New("node is offline: %s", node.Id.String()))
			continue
		}

		orderLimit, err := signing.SignOrderLimit(service.satellite, &pb.OrderLimit2{
			SerialNumber:    serialNumber,
			SatelliteId:     service.satellite.ID(),
			UplinkId:        repairer.ID,
			StorageNodeId:   piece.NodeId,
			PieceId:         rootPieceID.Derive(piece.NodeId),
			Action:          pb.PieceAction_GET_REPAIR,
			Limit:           pieceSize,
			PieceExpiration: expiration,
			OrderExpiration: orderExpiration,
		})
		if err != nil {
			return nil, Error.Wrap(err)
		}

		limits[piece.GetPieceNum()] = &pb.AddressedOrderLimit{
			Limit:              orderLimit,
			StorageNodeAddress: node.Address,
		}
		limitsCount++
	}

	if limitsCount < redundancy.RequiredCount() {
		err = Error.New("not enough nodes available: got %d, required %d", limitsCount, redundancy.RequiredCount())
		return nil, errs.Combine(err, combinedErrs)
	}

	err = service.saveSerial(ctx, serialNumber, bucketID, orderExpirationTime)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	if err := service.updateBandwidth(ctx, bucketID, limits); err != nil {
		return nil, Error.Wrap(err)
	}

	return limits, nil
}

// CreatePutRepairOrderLimits creates the order limits for uploading the repaired pieces of pointer to newNodes.
func (service *Service) CreatePutRepairOrderLimits(ctx context.Context, repairer *identity.PeerIdentity, bucketID []byte, pointer *pb.Pointer, getOrderLimits []*pb.AddressedOrderLimit, newNodes []*pb.Node) (_ []*pb.AddressedOrderLimit, err error) {
	rootPieceID := pointer.GetRemote().RootPieceId
	redundancy, err := eestream.NewRedundancyStrategyFromProto(pointer.GetRemote().GetRedundancy())
	if err != nil {
		return nil, Error.Wrap(err)
	}
	pieceSize := eestream.CalcPieceSize(pointer.GetSegmentSize(), redundancy)
	totalPieces := redundancy.TotalCount()
	expiration := pointer.ExpirationDate

	// convert orderExpiration from duration to timestamp
	orderExpirationTime := time.Now().UTC().Add(service.orderExpiration)
	orderExpiration, err := ptypes.TimestampProto(orderExpirationTime)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	serialNumber, err := service.createSerial(ctx)
	if err != nil {
		return nil, err
	}

	limits := make([]*pb.AddressedOrderLimit, totalPieces)
	var pieceNum int
	for _, node := range newNodes {
		if node != nil {
			node.Type.DPanicOnInvalid("order service put repair order limits")
		}

		for pieceNum < totalPieces && getOrderLimits[pieceNum] != nil {
			pieceNum++
		}

		if pieceNum >= totalPieces { // should not happen
			return nil, Error.New("piece num greater than total pieces: %d >= %d", pieceNum, totalPieces)
		}

		orderLimit, err := signing.SignOrderLimit(service.satellite, &pb.OrderLimit2{
			SerialNumber:    serialNumber,
			SatelliteId:     service.satellite.ID(),
			UplinkId:        repairer.ID,
			StorageNodeId:   node.Id,
			PieceId:         rootPieceID.Derive(node.Id),
			Action:          pb.PieceAction_PUT_REPAIR,
			Limit:           pieceSize,
			PieceExpiration: expiration,
			OrderExpiration: orderExpiration,
		})
		if err != nil {
			return nil, Error.Wrap(err)
		}

		limits[pieceNum] = &pb.AddressedOrderLimit{
			Limit:              orderLimit,
			StorageNodeAddress: node.Address,
		}
		pieceNum++
	}

	err = service.saveSerial(ctx, serialNumber, bucketID, orderExpirationTime)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	if err := service.updateBandwidth(ctx, bucketID, limits); err != nil {
		return nil, Error.Wrap(err)
	}

	return limits, nil
}

// UpdateGetInlineOrder updates amount of inline GET bandwidth for given bucket
func (service *Service) UpdateGetInlineOrder(ctx context.Context, bucketID []byte, amount int64) (err error) {
	now := time.Now().UTC()
	intervalStart := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())

	return service.orders.UpdateBucketBandwidthInline(ctx, bucketID, pb.PieceAction_GET, amount, intervalStart)
}

// UpdatePutInlineOrder updates amount of inline PUT bandwidth for given bucket
func (service *Service) UpdatePutInlineOrder(ctx context.Context, bucketID []byte, amount int64) (err error) {
	now := time.Now().UTC()
	intervalStart := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())

	return service.orders.UpdateBucketBandwidthInline(ctx, bucketID, pb.PieceAction_PUT, amount, intervalStart)
}
