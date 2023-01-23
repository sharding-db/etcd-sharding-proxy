package server

import (
	"bytes"

	"github.com/pkg/errors"
	"github.com/sharding-db/etcd-sharding-proxy/pkg/config"
)

type Shard interface {
	Contains(key []byte, rangeEnd []byte) bool
	GetClient() ShardClient
}

// ShardImpl is the implementation of Shard
type ShardImpl struct {
	// start key of the range, inclusive.
	start []byte
	// end key of the range, exclusive.
	end []byte
	cli ShardClient
}

func NewShardImpl(shardID int, totalShards int, conf config.Shard) (*ShardImpl, error) {
	ret := new(ShardImpl)
	if len(conf.Start) > 0 {
		ret.start = []byte(conf.Start)
	} else {
		ret.start = conf.StartBytes
	}
	if shardID == 0 {
		ret.start = []byte{'\x00'}
	}

	if len(conf.End) > 0 {
		ret.end = []byte(conf.End)
	} else {
		ret.end = conf.EndBytes
	}
	if shardID == totalShards-1 {
		ret.end = []byte{'\xff'}
	}

	var err error
	ret.cli, err = NewShardClientImpl(shardID, conf.Address)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create shard client to [%s]", conf.Address)
	}
	return ret, nil
}

// Contains returns true if the key is in the range of the shard.
func (s *ShardImpl) Contains(key []byte, rangeEnd []byte) bool {
	if bytes.Compare(key, s.end) >= 0 {
		return false
	}
	if bytes.Compare(key, s.start) > 0 {
		return true
	}
	if len(rangeEnd) < 1 {
		return false
	}
	if bytes.Compare(rangeEnd, s.start) > 0 {
		return true
	}
	return false
}

// GetClient returns the client of the shard.
func (s *ShardImpl) GetClient() ShardClient {
	return s.cli
}
