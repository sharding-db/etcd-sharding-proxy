package server

import pb "go.etcd.io/etcd/api/v3/etcdserverpb"

type ProxyWatchClientFactory interface {
	// NewProxyWatchClient add a watch by WatchCreateRequest & shard client
	NewProxyWatchClient(req *pb.WatchCreateRequest, wc pb.Watch_WatchClient) ProxyWatchClient
	// Progress return watchresponse of all watch id
	Progress() []*pb.WatchResponse
	// Cancel a watch
	Cancel(req *pb.WatchRequest_CancelRequest)
}

type ProxyWatchClientFactoryImpl struct {
	// watchCliSet is a map of watchId to ProxyWatchClientSet
	watchCliSet map[int64][]ProxyWatchClient
}

func NewProxyWatchClientFactory() ProxyWatchClientFactory

func (p *ProxyWatchClientFactoryImpl) NewProxyWatchClient(req *pb.WatchCreateRequest, shardWatchCli pb.Watch_WatchClient) {
	// TODO: make pwc
	watchId := req.WatchId
	pwc := NewProxyWatchClient(shardWatchCli)
	pwc.Run(req)
	if _, found := p.watchCliSet[watchId]; !found {
		p.watchCliSet[watchId] = make([]ProxyWatchClient, 0, 1)
	}
	p.watchCliSet[watchId] = append(p.watchCliSet[watchId], pwc)
}

func (p *ProxyWatchClientFactoryImpl) Progress() []*pb.WatchResponse {
	// TODO:
	return []*pb.WatchResponse{}
}

func (p *ProxyWatchClientFactoryImpl) Cancel(req *pb.WatchRequest_CancelRequest) {
	clis, found := p.watchCliSet[req.CancelRequest.WatchId]
	if !found {
		// TODO:
		return
	}
	for _, cli := range clis {
		cli.Cancel(req)
	}
	delete(p.watchCliSet, req.CancelRequest.WatchId)
}

type ProxyWatchClient interface {
	Run(req *pb.WatchCreateRequest)
	Cancel(req *pb.WatchRequest_CancelRequest)
}

type ProxyWatchClientImpl struct {
}

func NewProxyWatchClient(shardWatchCli pb.Watch_WatchClient) ProxyWatchClient {
	return &ProxyWatchClientImpl{}
}

func (p *ProxyWatchClientImpl) Run(req *pb.WatchCreateRequest) {
}

func (p *ProxyWatchClientImpl) Cancel(req *pb.WatchRequest_CancelRequest)
