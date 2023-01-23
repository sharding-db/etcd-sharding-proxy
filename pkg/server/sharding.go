package server

import (
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

type ShardClient interface {
	pb.KVClient
	GetShardID() int
}

type ShardingConfigs interface {
	GetShardClis(key []byte, rangeEnd []byte) []ShardClient
}

type DefaultShardingConfigs struct {
	shards []Shard
}

func NewDefaultShardingConfigs(shards []Shard) *DefaultShardingConfigs {
	return &DefaultShardingConfigs{
		shards: shards,
	}
}

func (d *DefaultShardingConfigs) GetShardClis(key []byte, rangeEnd []byte) []ShardClient {
	var ret = make([]ShardClient, 0, len(d.shards))
	for _, shard := range d.shards {
		if shard.Contains(key, rangeEnd) {
			ret = append(ret, shard.GetClient())
		} else {
			break
		}
	}
	return ret
}
