package server

import (
	"context"

	"github.com/pkg/errors"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

var _ pb.KVServer = &ShardingProxy{}
var _ pb.WatchServer = &ShardingProxy{}

type ShardingProxy struct {
	watchStreams ProxyWatchStreamFactory
	groupRunners GroupRunnerFactory
	configs      ShardingConfigs
	respFilter   ResponseFilter
}

func NewShardingProxy(groupRunners GroupRunnerFactory, configs ShardingConfigs, respFilter ResponseFilter) *ShardingProxy {
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
	var err error
	if len(rets) > 1 {
		groupRunner := s.groupRunners.GetGroupRunner()
		for i := range shardClis {
			index := i
			groupRunner.Go(func() error {
				rets[index], err = shardClis[index].Range(ctx, req)
				return errors.Wrapf(err, "failed to do range in shard[%d]", shardClis[index].GetShardID())
			})
		}
		err := groupRunner.Wait()
		if err != nil {
			return nil, err
		}
	} else {
		rets[0], err = shardClis[0].Range(ctx, req)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to do range in shard[%d]", shardClis[0].GetShardID())
		}
	}
	ret, err := s.respFilter.FilterRange(req, rets)
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
func (s *ShardingProxy) DeleteRange(ctx context.Context, req *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, error) {
	shardClis := s.configs.GetShardClis(req.Key, req.RangeEnd)
	var rets = make([]*pb.DeleteRangeResponse, len(shardClis))
	var err error
	groupRunner := s.groupRunners.GetGroupRunner()
	if len(rets) > 1 {
		for i := range shardClis {
			index := i
			groupRunner.Go(func() error {
				rets[index], err = shardClis[index].DeleteRange(ctx, req)
				if err != nil {
					return errors.Wrapf(err, "failed to do delete range in shard[%d]", shardClis[index].GetShardID())
				}
				return nil
			})
		}
		err = groupRunner.Wait()
	} else {
		rets[0], err = shardClis[0].DeleteRange(ctx, req)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to do delete range in shard[%d]", shardClis[0].GetShardID())
		}
	}
	if err != nil {
		return nil, err
	}
	ret, err := s.respFilter.FilterDeleteRange(rets)
	if err != nil {
		return nil, errors.Wrap(err, "failed to filter delete range response")
	}
	return ret, nil
}

// Txn processes multiple requests in a single transaction.
// A txn request increments the revision of the key-value store
// and generates events with the same revision for every completed request.
// It is not allowed to modify the same key several times within one txn.
func (s *ShardingProxy) Txn(ctx context.Context, req *pb.TxnRequest) (*pb.TxnResponse, error) {
	shardID, err := s.getShardID(req)
	if err != nil {
		return nil, err
	}
	return s.configs.GetShardCli(shardID).Txn(ctx, req)
}

// Watch watches for events happening or that have happened. Both input and output
// are streams; the input stream is for creating and canceling watchers and the output
// stream sends events. One watch RPC can watch on multiple key ranges, streaming events
// for several watches at once. The entire event history can be watched starting from the
// last compaction revision.
func (s *ShardingProxy) Watch(stream pb.Watch_WatchServer) (err error) {
	return s.watchStreams.NewProxyWatchStream(stream, s.configs).
		Run()
}

func (s *ShardingProxy) getShardID(req *pb.TxnRequest) (int, error) {
	var getShardIDByOp = func(op *pb.RequestOp) (int, error) {
		rangeOp := op.GetRequestRange()
		if rangeOp != nil {
			clis := s.configs.GetShardClis(rangeOp.Key, nil)
			return clis[0].GetShardID(), nil
		}
		putOp := op.GetRequestPut()
		if putOp != nil {
			clis := s.configs.GetShardClis(putOp.Key, nil)
			return clis[0].GetShardID(), nil
		}
		deleteOp := op.GetRequestDeleteRange()
		if deleteOp != nil {
			clis := s.configs.GetShardClis(deleteOp.Key, nil)
			return clis[0].GetShardID(), nil
		}
		// assume txn
		return s.getShardID(op.GetRequestTxn())
	}
	for _, op := range req.Success {
		return getShardIDByOp(op)
	}
	for _, op := range req.Failure {
		return getShardIDByOp(op)
	}
	// empty txn, use first shard
	return 0, nil
}

var ErrNotSupported = errors.New("not supported")
var ErrTxnDifferentShard = errors.Wrap(ErrNotSupported, "txn in different shard")

// Compact compacts the event history in the etcd key-value store. The key-value
// store should be periodically compacted or the event history will continue to grow
// indefinitely.
func (s *ShardingProxy) Compact(ctx context.Context, req *pb.CompactionRequest) (*pb.CompactionResponse, error) {
	return nil, ErrNotSupported
}

type ResponseFilter interface {
	FilterRange(req *pb.RangeRequest, resps []*pb.RangeResponse) (*pb.RangeResponse, error)
	FilterDeleteRange([]*pb.DeleteRangeResponse) (*pb.DeleteRangeResponse, error)
}

type DefaultResponseFilter struct {
}

func (DefaultResponseFilter) FilterRange(req *pb.RangeRequest, resps []*pb.RangeResponse) (*pb.RangeResponse, error) {
	// assume len(resps) >= 1
	if len(resps) < 2 {
		return resps[0], nil
	}
	ret := resps[0]
	for i := 1; i < len(resps); i++ {
		if req.Limit > 0 && ret.Count >= req.Limit {
			ret.More = true
			break
		}
		if resps[i].Count > 0 {
			if ret.Kvs == nil {
				ret.Kvs = resps[i].Kvs
			}
			ret.Count += resps[i].Count
			ret.Kvs = append(ret.Kvs, resps[i].Kvs...)
		}
		if resps[i].More {
			ret.More = true
		}
	}
	return ret, nil
}

func (DefaultResponseFilter) FilterDeleteRange(resps []*pb.DeleteRangeResponse) (*pb.DeleteRangeResponse, error) {
	if len(resps) == 0 {
		return nil, errors.New("no response")
	}
	ret := resps[0]
	for i := 1; i < len(resps); i++ {
		ret.Deleted += resps[i].Deleted
	}
	return ret, nil
}
