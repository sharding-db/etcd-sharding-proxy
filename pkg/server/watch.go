package server

import (
	"context"
	"io"
	"strings"

	"github.com/pkg/errors"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ProxyWatchStreamFactory interface {
	NewProxyWatchStream(gRPCStream pb.Watch_WatchServer, sharding ShardingConfigs) ProxyWatchStream
}

type ProxyWatchStreamFactoryImpl struct {
}

func (ProxyWatchStreamFactoryImpl) NewProxyWatchStream(gRPCStream pb.Watch_WatchServer, sharding ShardingConfigs) ProxyWatchStream {
	return NewProxyWatchStreamImpl(gRPCStream, sharding)
}

type ProxyWatchStream interface {
	Run() error
}

// ProxyWatchStreamImpl implements ProxyWatchStream
type ProxyWatchStreamImpl struct {
	ctx        context.Context
	cancel     context.CancelFunc
	lg         *zap.Logger
	gRPCStream pb.Watch_WatchServer
	configs    ShardingConfigs
	// TODO:
	callOpts []grpc.CallOption

	groupRunner    GroupRunner
	mapShardStream map[int]pb.Watch_WatchClient

	recvChan        chan *pb.WatchRequest
	respChan        chan *pb.WatchResponse
	upstreamErrChan chan error
}

func NewProxyWatchStreamImpl(gRPCStream pb.Watch_WatchServer, sharding ShardingConfigs) *ProxyWatchStreamImpl {
	upstreamErrChan := make(chan error, 1)
	ctx, cancel := context.WithCancel(gRPCStream.Context())
	return &ProxyWatchStreamImpl{
		ctx:         ctx,
		cancel:      cancel,
		lg:          zap.L().Named("ProxyWatchStream"),
		gRPCStream:  gRPCStream,
		configs:     sharding,
		groupRunner: new(errgroup.Group),

		mapShardStream: make(map[int]pb.Watch_WatchClient),
		// shardClientFactory: NewProxyWatchClientFactory(ctx, upstreamErrChan),

		recvChan:        make(chan *pb.WatchRequest, 10),
		respChan:        make(chan *pb.WatchResponse, 10),
		upstreamErrChan: upstreamErrChan,
	}
}

func (p *ProxyWatchStreamImpl) Run() error {
	p.groupRunner.Go(p.sendLoop)
	p.groupRunner.Go(p.recvLoop)
	p.groupRunner.Go(p.handleRecvLoop)
	return p.groupRunner.Wait()
}

func (p *ProxyWatchStreamImpl) recvLoop() error {
	for {
		req, err := p.gRPCStream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			p.cancel()
			return err
		}
		p.recvChan <- req
	}
}

func (p *ProxyWatchStreamImpl) handleRecvLoop() error {
	for {
		var req *pb.WatchRequest
		var err error
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case err = <-p.upstreamErrChan:
			return errors.Wrap(err, "upstream error")
		case req = <-p.recvChan:
		}

		for _, shardCli := range p.configs.GetAllShardClis() {
			shardStream, exist := p.mapShardStream[shardCli.GetShardID()]
			if !exist {
				shardStream, err = shardCli.Watch(p.ctx, p.callOpts...)
				if err != nil {
					return errors.Wrapf(err, "watch on shard[%d]", shardCli.GetShardID())
				}
				go func() {
					for {
						resp, err := shardStream.Recv()
						if err != nil {
							if err == io.EOF {
								return
							}
							p.lg.Warn("failed to receive watch response from shard stream", zap.Error(err))
						}
						p.respChan <- resp
					}
				}()
				p.mapShardStream[shardCli.GetShardID()] = shardStream
			}
			shardStream.Send(req)
		}
	}
}

func (p *ProxyWatchStreamImpl) sendLoop() error {
	var msg *pb.WatchResponse
	for {
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case msg = <-p.respChan:
		}
		err := p.gRPCStream.Send(msg)
		if err != nil {
			if !isClientCtxErr(p.gRPCStream.Context().Err(), err) {
				p.lg.Warn("failed to send watch control response to gRPC stream", zap.Error(err))
			}
			return err
		}
	}
}

func isClientCtxErr(ctxErr error, err error) bool {
	if ctxErr != nil {
		return true
	}

	ev, ok := status.FromError(err)
	if !ok {
		return false
	}

	switch ev.Code() {
	case codes.Canceled, codes.DeadlineExceeded:
		// client-side context cancel or deadline exceeded
		// "rpc error: code = Canceled desc = context canceled"
		// "rpc error: code = DeadlineExceeded desc = context deadline exceeded"
		return true
	case codes.Unavailable:
		msg := ev.Message()
		// client-side context cancel or deadline exceeded with TLS ("http2.errClientDisconnected")
		// "rpc error: code = Unavailable desc = client disconnected"
		if msg == "client disconnected" {
			return true
		}
		// "grpc/transport.ClientTransport.CloseStream" on canceled streams
		// "rpc error: code = Unavailable desc = stream error: stream ID 21; CANCEL")
		if strings.HasPrefix(msg, "stream error: ") && strings.HasSuffix(msg, "; CANCEL") {
			return true
		}
	}
	return false
}
