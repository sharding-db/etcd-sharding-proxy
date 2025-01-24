package server

import (
	"context"
	"io"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"golang.org/x/sync/errgroup"
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
			ret = resp
		}
		ret.Keys = append(resp.Keys, resp.Keys...)
	}
	return ret, nil
}

func (p *LeaseProxy) LeaseLeases(ctx context.Context, in *pb.LeaseLeasesRequest) (ret *pb.LeaseLeasesResponse, err error) {
	for _, shardCli := range p.configs.GetAllShardClis() {
		resp, err := shardCli.LeaseLeases(ctx, in)
		if err != nil {
			return nil, err
		}
		if ret == nil {
			ret = resp
		}
		ret.Leases = append(resp.Leases, resp.Leases...)
	}
	return ret, nil
}

func (p *LeaseProxy) LeaseKeepAlive(stream pb.Lease_LeaseKeepAliveServer) error {
	keepAliveStream := NewSingleLeaseKeepAliveProxy(p.configs, stream)
	return keepAliveStream.Run()
}

type SingleLeaseKeepAliveProxy struct {
	configs ShardingConfigs
	ctx     context.Context
	stream  pb.Lease_LeaseKeepAliveServer
	cancel  context.CancelFunc

	groupRunner    GroupRunner
	mapShardStream map[int]pb.Lease_LeaseKeepAliveClient
	recvChan       chan *pb.LeaseKeepAliveRequest
	respChan       chan *pb.LeaseKeepAliveResponse
}

func NewSingleLeaseKeepAliveProxy(configs ShardingConfigs, stream pb.Lease_LeaseKeepAliveServer) *SingleLeaseKeepAliveProxy {
	ctx, cancel := context.WithCancel(stream.Context())
	return &SingleLeaseKeepAliveProxy{
		configs:        configs,
		ctx:            ctx,
		stream:         stream,
		cancel:         cancel,
		groupRunner:    new(errgroup.Group),
		mapShardStream: make(map[int]pb.Lease_LeaseKeepAliveClient),
		recvChan:       make(chan *pb.LeaseKeepAliveRequest, 10),
		respChan:       make(chan *pb.LeaseKeepAliveResponse, 10),
	}
}

func (l *SingleLeaseKeepAliveProxy) Run() error {
	l.groupRunner.Go(l.sendLoop)
	l.groupRunner.Go(l.recvLoop)
	l.groupRunner.Go(l.handleRecvLoop)
	return l.groupRunner.Wait()
}

func (l *SingleLeaseKeepAliveProxy) sendLoop() error {
	for {
		select {
		case <-l.ctx.Done():
			return l.ctx.Err()
		case resp := <-l.respChan:
			err := l.stream.Send(resp)
			if err != nil {
				return err
			}
		}
	}
}

func (l *SingleLeaseKeepAliveProxy) recvLoop() error {
	for {
		req, err := l.stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			l.cancel()
			return err
		}
		l.recvChan <- req
	}
}

func (p *SingleLeaseKeepAliveProxy) handleRecvLoop() error {
	for {
		var req *pb.LeaseKeepAliveRequest
		var err error
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case req = <-p.recvChan:
		}

		for _, shardCli := range p.configs.GetAllShardClis() {
			shardStream, exist := p.mapShardStream[shardCli.GetShardID()]
			if !exist {
				shardStream, err = shardCli.LeaseKeepAlive(p.ctx)
				if err != nil {
					if err == io.EOF {
						return nil
					}
				}
				p.mapShardStream[shardCli.GetShardID()] = shardStream
			}
			shardStream.Send(req)
		}
	}
}
