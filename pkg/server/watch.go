package server

import (
	"context"
	"io"
	"strings"

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
	lg         *zap.Logger
	gRPCStream pb.Watch_WatchServer
	configs    ShardingConfigs
	// TODO:
	callOpts []grpc.CallOption

	groupRunner   GroupRunner
	clientFactory ProxyWatchClientFactory

	recvChan   chan recvChanMsg
	respChan   chan *pb.WatchResponse
	ctrlStream chan *pb.WatchResponse
}

func NewProxyWatchStreamImpl(gRPCStream pb.Watch_WatchServer, sharding ShardingConfigs) *ProxyWatchStreamImpl {
	return &ProxyWatchStreamImpl{
		ctx:           gRPCStream.Context(),
		lg:            zap.L().Named("ProxyWatchStream"),
		gRPCStream:    gRPCStream,
		configs:       sharding,
		groupRunner:   new(errgroup.Group),
		clientFactory: NewProxyWatchClientFactory(),

		recvChan:   make(chan recvChanMsg, 10),
		respChan:   make(chan *pb.WatchResponse, 10),
		ctrlStream: make(chan *pb.WatchResponse, 10),
	}
}

func (p *ProxyWatchStreamImpl) Run() error {
	p.groupRunner.Go(p.sendLoop)
	p.groupRunner.Go(p.recvLoop)
	p.groupRunner.Go(p.handleRecvLoop)
	return p.groupRunner.Wait()
}

type recvChanMsg struct {
	Req *pb.WatchRequest
	Err error
}

func (p *ProxyWatchStreamImpl) recvLoop() error {
	for {
		req, err := p.gRPCStream.Recv()
		p.recvChan <- recvChanMsg{Req: req, Err: err}
		if err != nil {
			return nil
		}
	}
}

func (p *ProxyWatchStreamImpl) handleRecvLoop() error {
	for {
		var req *pb.WatchRequest
		var err error
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-p.recvChan:
		}

		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		switch uv := req.RequestUnion.(type) {
		case *pb.WatchRequest_CreateRequest:
			if uv.CreateRequest == nil {
				break
			}
			creq := uv.CreateRequest
			clis := p.configs.GetShardClis(creq.Key, creq.RangeEnd)
			for _, cli := range clis {
				clientStream, err := cli.Watch(p.ctx, p.callOpts...)
				if err != nil {
					return err
				}
				p.clientFactory.NewProxyWatchClient(creq, clientStream)
			}

		case *pb.WatchRequest_CancelRequest:
			if uv.CancelRequest != nil {
				p.clientFactory.Cancel(uv)
			}

		case *pb.WatchRequest_ProgressRequest:
			if uv.ProgressRequest != nil {
				// return watch response of all watch id
				resps := p.clientFactory.Progress()
				for _, resp := range resps {
					p.ctrlStream <- resp
				}
			}

		default:
			// we probably should not shutdown the entire stream when
			// receive an valid command.
			// so just do nothing instead.
			continue
		}
	}
}

func (p *ProxyWatchStreamImpl) sendLoop() error {
	var msg *pb.WatchResponse
	for {
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case msg = <-p.ctrlStream:
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
