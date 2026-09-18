package server

import (
	"context"
	"io"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ pb.LeaseServer = &LeaseProxy{}

type LeaseProxy struct {
	configs ShardingConfigs
}

func NewLeaseProxy(configs ShardingConfigs) *LeaseProxy {
	return &LeaseProxy{
		configs: configs,
	}
}

func (p *LeaseProxy) LeaseGrant(ctx context.Context, in *pb.LeaseGrantRequest) (ret *pb.LeaseGrantResponse, err error) {
	if in.ID == 0 {
		in.ID = GetIDGenerator().NextInt64()
	}
	for _, shardCli := range p.configs.GetAllShardClis() {
		resp, err := shardCli.LeaseGrant(ctx, in)
		if err != nil {
			return nil, err
		}
		if ret == nil {
			ret = resp
		}
	}
	return ret, nil
}

func (p *LeaseProxy) LeaseRevoke(ctx context.Context, in *pb.LeaseRevokeRequest) (ret *pb.LeaseRevokeResponse, err error) {
	for _, shardCli := range p.configs.GetAllShardClis() {
		resp, err := shardCli.LeaseRevoke(ctx, in)
		if err != nil {
			return nil, err
		}
		if ret == nil {
			ret = resp
		}
	}
	return ret, nil
}

func (p *LeaseProxy) LeaseTimeToLive(ctx context.Context, in *pb.LeaseTimeToLiveRequest) (ret *pb.LeaseTimeToLiveResponse, err error) {
	for _, shardCli := range p.configs.GetAllShardClis() {
		resp, err := shardCli.LeaseTimeToLive(ctx, in)
		if err != nil {
			return nil, err
		}
		if ret == nil {
			ret = &pb.LeaseTimeToLiveResponse{Header: resp.Header, ID: resp.ID, TTL: resp.TTL, GrantedTTL: resp.GrantedTTL}
		}
		if resp.TTL < ret.TTL {
			ret.TTL = resp.TTL
		}
		ret.Keys = append(ret.Keys, resp.Keys...)
	}
	return ret, nil
}

// LeaseLeases lists the first shard: leases are replicated with the same ID.
func (p *LeaseProxy) LeaseLeases(ctx context.Context, in *pb.LeaseLeasesRequest) (*pb.LeaseLeasesResponse, error) {
	for _, shardCli := range p.configs.GetAllShardClis() {
		return shardCli.LeaseLeases(ctx, in)
	}
	return nil, status.Error(codes.Unavailable, "no lease shards configured")
}

func (p *LeaseProxy) LeaseKeepAlive(stream pb.Lease_LeaseKeepAliveServer) error {
	keepAliveStream := NewSingleLeaseKeepAliveProxy(p.configs, stream)
	return keepAliveStream.Run()
}

type SingleLeaseKeepAliveProxy struct {
	configs ShardingConfigs
	stream  pb.Lease_LeaseKeepAliveServer
}

func NewSingleLeaseKeepAliveProxy(configs ShardingConfigs, stream pb.Lease_LeaseKeepAliveServer) *SingleLeaseKeepAliveProxy {
	return &SingleLeaseKeepAliveProxy{configs: configs, stream: stream}
}

func (p *SingleLeaseKeepAliveProxy) Run() error {
	ctx, cancel := context.WithCancel(p.stream.Context())
	defer cancel()
	shards := p.configs.GetAllShardClis()
	if len(shards) == 0 {
		return status.Error(codes.Unavailable, "no lease shards configured")
	}
	// Downstream Recv is canceled by gRPC when this handler returns. Do not
	// wait for it on a backend failure: the client may not send another request.
	type received struct {
		request *pb.LeaseKeepAliveRequest
		err     error
	}
	requests := make(chan received)
	go func() {
		for {
			req, err := p.stream.Recv()
			select {
			case requests <- received{req, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	streams := make([]pb.Lease_LeaseKeepAliveClient, len(shards))
	for {
		var next received
		select {
		case <-ctx.Done():
			return ctx.Err()
		case next = <-requests:
		}
		if next.err == io.EOF {
			return nil
		}
		if next.err != nil {
			return next.err
		}
		// Send to every shard before receiving so renewals do not wait for replies.
		for i, shard := range shards {
			if streams[i] == nil {
				backend, err := shard.LeaseKeepAlive(ctx)
				if err != nil {
					return err
				}
				streams[i] = backend
			}
			if err := streams[i].Send(next.request); err != nil {
				// Send reports EOF when the server has terminated the stream;
				// Recv carries the actual terminal gRPC status.
				if err == io.EOF {
					_, err = streams[i].Recv()
					if err == nil || err == io.EOF {
						err = status.Error(codes.Unavailable, "lease shard closed keepalive stream")
					}
				}
				return err
			}
		}
		var response *pb.LeaseKeepAliveResponse
		for _, backend := range streams {
			resp, err := backend.Recv()
			if err != nil {
				if err == io.EOF {
					return status.Error(codes.Unavailable, "lease shard closed keepalive stream")
				}
				return err
			}
			if resp.ID != next.request.ID {
				return status.Error(codes.Internal, "unexpected lease keepalive response ID")
			}
			// A logical lease is alive only as long as its shortest-lived shard.
			if response == nil || resp.TTL < response.TTL {
				response = resp
			}
		}
		if err := p.stream.Send(response); err != nil {
			return err
		}
	}
}
