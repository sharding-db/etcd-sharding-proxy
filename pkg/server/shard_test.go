package server

import (
	"testing"

	"github.com/sharding-db/etcd-sharding-proxy/pkg/config"
	"github.com/stretchr/testify/assert"
)

func TestShardImpl_Contains(t *testing.T) {
	t.Run("first shard", func(t *testing.T) {
		shard, err := NewShardImpl(0, 3, config.Shard{
			Start:   "",
			End:     "i",
			Address: "127.0.0.1:2379",
		})
		assert.NoError(t, err)
		assert.True(t, shard.Contains([]byte("a"), nil))
		assert.False(t, shard.Contains([]byte("i"), nil))
	})

	t.Run("last shard", func(t *testing.T) {
		shard, err := NewShardImpl(2, 3, config.Shard{
			Start:   "s",
			End:     "",
			Address: "127.0.0.1:2379",
		})
		assert.NoError(t, err)
		assert.False(t, shard.Contains([]byte("r"), nil))
		assert.True(t, shard.Contains([]byte("s"), nil))
		assert.True(t, shard.Contains([]byte("r"), []byte{0}))
	})

	t.Run("middle shard", func(t *testing.T) {
		shard, err := NewShardImpl(1, 3, config.Shard{
			Start:   "i",
			End:     "s",
			Address: "127.0.0.1:2379",
		})
		assert.NoError(t, err)
		assert.False(t, shard.Contains([]byte("h"), nil))
		assert.True(t, shard.Contains([]byte("i"), nil))
	})
}
