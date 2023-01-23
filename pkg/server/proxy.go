package server

import (
	"context"

	"github.com/pkg/errors"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

var _ pb.KVServer = &ShardingProxy{}

type ShardingProxy struct {
	groupRunners GroupRunnerFactory
	configs      ShardingConfigs
	respFilter   RangeResponseFilter
}

func NewShardingProxy(groupRunners GroupRunnerFactory, configs ShardingConfigs, respFilter RangeResponseFilter) *ShardingProxy {
	return &ShardingProxy{
		groupRunners: groupRunners,
		configs:      configs,
		respFilter:   respFilter,
	}
}

// Range gets the keys in the range from the key-value store.
func (s *ShardingProxy) Range(ctx context.Context, req *pb.RangeRequest) (*pb.RangeResponse, error) {
	shardClis := s.configs.GetShardClis(req.Key, req.RangeEnd)
	var rets = make([]*pb.RangeResponse, len(shardClis))
	groupRunner := s.groupRunners.GetGroupRunner()
	for i, shardCli := range shardClis {
		groupRunner.Add(func() error {
			var err error
			rets[i], err = shardCli.Range(ctx, req)
			if err != nil {
				return errors.Wrapf(err, "failed to do range in shard[%d]", shardCli.GetShardID())
			}
			return nil
		})
	}
	err := groupRunner.Do()
	if err != nil {
		return nil, err
	}
	ret, err := s.respFilter.Filter(rets)
	if err != nil {
		return nil, errors.Wrap(err, "failed to filter range response")
	}
	return ret, nil
}

// Put puts the given key into the key-value store.
// A put request increments the revision of the key-value store
// and generates one event in the event history.
func (s *ShardingProxy) Put(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error) {
	shardCli := s.configs.GetShardClis(req.Key, nil)[0]
	return shardCli.Put(ctx, req)
}

// DeleteRange deletes the given range from the key-value store.
// A delete request increments the revision of the key-value store
// and generates a delete event in the event history for every deleted key.
func (s *ShardingProxy) DeleteRange(context.Context, *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, error) {
	panic("implement me")
}

// Txn processes multiple requests in a single transaction.
// A txn request increments the revision of the key-value store
// and generates events with the same revision for every completed request.
// It is not allowed to modify the same key several times within one txn.
func (s *ShardingProxy) Txn(context.Context, *pb.TxnRequest) (*pb.TxnResponse, error) {
	panic("implement me")
}

// Compact compacts the event history in the etcd key-value store. The key-value
// store should be periodically compacted or the event history will continue to grow
// indefinitely.
func (s *ShardingProxy) Compact(context.Context, *pb.CompactionRequest) (*pb.CompactionResponse, error) {
	panic("implement me")
}

type RangeResponseFilter interface {
	Filter([]*pb.RangeResponse) (*pb.RangeResponse, error)
}

type DefaultRangeResponseFilter struct {
}

func (DefaultRangeResponseFilter) Filter(resps []*pb.RangeResponse) (*pb.RangeResponse, error) {
	if len(resps) == 0 {
		return nil, errors.New("no response")
	}
	ret := resps[0]
	for i := 1; i < len(resps); i++ {
		ret.Kvs = append(ret.Kvs, resps[i].Kvs...)
		ret.Count += resps[i].Count
		if resps[i].More {
			ret.More = true
		}
	}
	return ret, nil
}