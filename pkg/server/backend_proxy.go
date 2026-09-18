package server

import (
	"context"
	"io"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// BackendProxy preserves one backend revision domain. The caller owns conn.
type BackendProxy struct {
	kv          pb.KVClient
	watch       pb.WatchClient
	lease       pb.LeaseClient
	maintenance pb.MaintenanceClient
}

var (
	_ pb.KVServer          = (*BackendProxy)(nil)
	_ pb.WatchServer       = (*BackendProxy)(nil)
	_ pb.LeaseServer       = (*BackendProxy)(nil)
	_ pb.MaintenanceServer = (*BackendProxy)(nil)
)

func NewBackendProxy(conn *grpc.ClientConn) *BackendProxy {
	return &BackendProxy{pb.NewKVClient(conn), pb.NewWatchClient(conn), pb.NewLeaseClient(conn), pb.NewMaintenanceClient(conn)}
}
func backendContext(ctx context.Context) context.Context {
	incoming, _ := metadata.FromIncomingContext(ctx)
	outgoing, _ := metadata.FromOutgoingContext(ctx)
	return metadata.NewOutgoingContext(ctx, metadata.Join(outgoing, incoming))
}
func forwardUnary[Req, Resp any](ctx context.Context, req *Req, call func(context.Context, *Req, ...grpc.CallOption) (*Resp, error)) (*Resp, error) {
	var header, trailer metadata.MD
	resp, err := call(backendContext(ctx), req, grpc.Header(&header), grpc.Trailer(&trailer))
	if len(header) != 0 {
		_ = grpc.SetHeader(ctx, header)
	}
	if len(trailer) != 0 {
		_ = grpc.SetTrailer(ctx, trailer)
	}
	return resp, err
}

func (p *BackendProxy) Range(ctx context.Context, req *pb.RangeRequest) (*pb.RangeResponse, error) {
	return forwardUnary(ctx, req, p.kv.Range)
}

func (p *BackendProxy) Put(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error) {
	return forwardUnary(ctx, req, p.kv.Put)
}

func (p *BackendProxy) DeleteRange(ctx context.Context, req *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, error) {
	return forwardUnary(ctx, req, p.kv.DeleteRange)
}

func (p *BackendProxy) Txn(ctx context.Context, req *pb.TxnRequest) (*pb.TxnResponse, error) {
	return forwardUnary(ctx, req, p.kv.Txn)
}

func (p *BackendProxy) Compact(ctx context.Context, req *pb.CompactionRequest) (*pb.CompactionResponse, error) {
	return forwardUnary(ctx, req, p.kv.Compact)
}

func (p *BackendProxy) LeaseGrant(ctx context.Context, req *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	return forwardUnary(ctx, req, p.lease.LeaseGrant)
}

func (p *BackendProxy) LeaseRevoke(ctx context.Context, req *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	return forwardUnary(ctx, req, p.lease.LeaseRevoke)
}

func (p *BackendProxy) LeaseTimeToLive(ctx context.Context, req *pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error) {
	return forwardUnary(ctx, req, p.lease.LeaseTimeToLive)
}

func (p *BackendProxy) LeaseLeases(ctx context.Context, req *pb.LeaseLeasesRequest) (*pb.LeaseLeasesResponse, error) {
	return forwardUnary(ctx, req, p.lease.LeaseLeases)
}

func (p *BackendProxy) Alarm(ctx context.Context, req *pb.AlarmRequest) (*pb.AlarmResponse, error) {
	return forwardUnary(ctx, req, p.maintenance.Alarm)
}

func (p *BackendProxy) Status(ctx context.Context, req *pb.StatusRequest) (*pb.StatusResponse, error) {
	return forwardUnary(ctx, req, p.maintenance.Status)
}

func (p *BackendProxy) Defragment(ctx context.Context, req *pb.DefragmentRequest) (*pb.DefragmentResponse, error) {
	return forwardUnary(ctx, req, p.maintenance.Defragment)
}

func (p *BackendProxy) Hash(ctx context.Context, req *pb.HashRequest) (*pb.HashResponse, error) {
	return forwardUnary(ctx, req, p.maintenance.Hash)
}

func (p *BackendProxy) HashKV(ctx context.Context, req *pb.HashKVRequest) (*pb.HashKVResponse, error) {
	return forwardUnary(ctx, req, p.maintenance.HashKV)
}

func (p *BackendProxy) MoveLeader(ctx context.Context, req *pb.MoveLeaderRequest) (*pb.MoveLeaderResponse, error) {
	return forwardUnary(ctx, req, p.maintenance.MoveLeader)
}

func (p *BackendProxy) Downgrade(ctx context.Context, req *pb.DowngradeRequest) (*pb.DowngradeResponse, error) {
	return forwardUnary(ctx, req, p.maintenance.Downgrade)
}

func (p *BackendProxy) Watch(front pb.Watch_WatchServer) error {
	ctx, cancel := context.WithCancel(backendContext(front.Context()))
	defer cancel()
	back, err := p.watch.Watch(ctx)
	if err != nil {
		return err
	}
	return forwardBidi(front, back, cancel, func() interface{} { return new(pb.WatchRequest) }, func() interface{} { return new(pb.WatchResponse) })
}

func (p *BackendProxy) LeaseKeepAlive(front pb.Lease_LeaseKeepAliveServer) error {
	ctx, cancel := context.WithCancel(backendContext(front.Context()))
	defer cancel()
	back, err := p.lease.LeaseKeepAlive(ctx)
	if err != nil {
		return err
	}
	return forwardBidi(front, back, cancel, func() interface{} { return new(pb.LeaseKeepAliveRequest) }, func() interface{} { return new(pb.LeaseKeepAliveResponse) })
}

// A half-close only closes the backend send direction. SendMsg EOF is not
// success: RecvMsg owns the authoritative terminal backend status.
func forwardBidi(front grpc.ServerStream, back grpc.ClientStream, cancel context.CancelFunc, request, response func() interface{}) error {
	sendError := make(chan error, 1)
	go func() {
		fail := func(err error) {
			if err != nil && err != io.EOF {
				sendError <- err
				cancel()
			}
		}
		for {
			req := request()
			if err := front.RecvMsg(req); err != nil {
				if err == io.EOF {
					err = back.CloseSend()
				}
				fail(err)
				return
			}
			if err := back.SendMsg(req); err != nil {
				fail(err)
				return
			}
		}
	}()
	// Keep all writes to the frontend in this handler. Canceling the backend
	// unblocks this receive path before headers/trailers are finalized.
	err := forwardResponses(front, back, response)
	select {
	case sendErr := <-sendError:
		return sendErr
	default:
		return err
	}
	// Handler return releases a pending front.RecvMsg through its gRPC context.
}

func forwardResponses(front grpc.ServerStream, back grpc.ClientStream, response func() interface{}) error {
	defer func() { front.SetTrailer(back.Trailer()) }()
	header, err := back.Header()
	if err != nil {
		return err
	}
	if len(header) != 0 {
		if err := front.SendHeader(header); err != nil {
			return err
		}
	}
	for {
		resp := response()
		if err := back.RecvMsg(resp); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := front.SendMsg(resp); err != nil {
			return err
		}
	}
}
func (p *BackendProxy) Snapshot(req *pb.SnapshotRequest, front pb.Maintenance_SnapshotServer) error {
	ctx, cancel := context.WithCancel(backendContext(front.Context()))
	defer cancel()
	back, err := p.maintenance.Snapshot(ctx, req)
	if err != nil {
		return err
	}
	return forwardResponses(front, back, func() interface{} { return new(pb.SnapshotResponse) })
}
