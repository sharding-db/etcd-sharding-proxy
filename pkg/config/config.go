package config

import (
	"log"

	"github.com/pkg/errors"
	"github.com/spf13/viper"
)

// Configurations is the configurations of the proxy
type Configurations struct {
	// ShardingRules is the sharding rules of the cluster.
	// start key of first shard & end key of last shard are ignored.
	Shards []Shard `json:"shards"`
}

// NewConfigurationsFromFile  creates a new Configurations from a file.
func NewConfigurationsFromFile(path string) (*Configurations, error) {
	ret := new(Configurations)
	viper.SetConfigFile(path)
	viper.SetConfigType("yaml")
	err := viper.ReadInConfig()
	if err != nil {
		return nil, errors.Wrap(err, "read config file failed")
	}
	err = viper.Unmarshal(ret)
	log.Println("config:", ret)
	return ret, errors.Wrap(err, "unmarshal config file failed")
}

// Shard is the configuration of one shard
// implements server.Shard
type Shard struct {
	// Start key of the range, inclusive.
	Start string `json:"start"`
	// StartBytes is the bytes of start key. Only used when start is empty.
	StartBytes []byte `json:"startBytes"`
	// End key of the range, exclusive.
	End string `json:"end"`
	// EndBytes is the bytes of end key. Only used when end is empty.
	EndBytes []byte `json:"endBytes"`
	// Address is the address of the shard. Address format is "host:port".
	Address string `json:"address"`
}
