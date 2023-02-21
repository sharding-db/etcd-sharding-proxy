package server

import (
	"github.com/pkg/errors"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type EtcdGrpcClient interface {
	// pb.ClusterClient
	pb.KVClient
	// pb.LeaseClient
	pb.WatchClient
	// pb.AuthClient
	// pb.MaintenanceClient
}

// EtcdGrpcClientImpl impl EtcdGrpcClient
type EtcdGrpcClientImpl struct {
	pb.KVClient
	pb.WatchClient
}

type ShardClientImpl struct {
	shardID int
	EtcdGrpcClient
}

func NewShardClientImpl(shardID int, address string) (*ShardClientImpl, error) {
	conn, err := grpc.Dial(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, errors.Wrap(err, "failed to dial etcd server")
	}
	kvCli := pb.NewKVClient(conn)
	watchCli := pb.NewWatchClient(conn)
	return &ShardClientImpl{
		shardID: shardID,
		EtcdGrpcClient: EtcdGrpcClientImpl{
			KVClient:    kvCli,
			WatchClient: watchCli,
		},
	}, nil
}

func (s *ShardClientImpl) GetShardID() int {
	return s.shardID
}
