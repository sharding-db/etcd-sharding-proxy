package server

import (
	"testing"

	"github.com/sharding-db/etcd-sharding-proxy/pkg/config"
	"github.com/stretchr/testify/assert"
)

func TestShardImpl_Contains(t *testing.T) {
	shard, err := NewShardImpl(0, 3, config.Shard{
		Start:   "",
		End:     "i",
		Address: "127.0.0.1:2379",
	})
	assert.NoError(t, err)
	assert.True(t, shard.Contains([]byte("a"), nil))
	assert.False(t, shard.Contains([]byte("i"), nil))
}
