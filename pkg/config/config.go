package config

import (
	"bytes"
	"fmt"

	"github.com/pkg/errors"
	"github.com/spf13/viper"
)

// Configurations is the configurations of the proxy
type Configurations struct {
	// ShardingRules is the sharding rules of the cluster.
	// start key of first shard & end key of last shard are ignored.
	Shards  []Shard  `json:"shards"`
	Backend *Backend `json:"backend"`
	TLS     TLS      `json:"tls"`
}

// NewConfigurationsFromFile  creates a new Configurations from a file.
func NewConfigurationsFromFile(path string) (*Configurations, error) {
	ret := new(Configurations)
	viper := viper.New()
	viper.SetConfigFile(path)
	viper.SetConfigType("yaml")
	err := viper.ReadInConfig()
	if err != nil {
		return nil, errors.Wrap(err, "read config file failed")
	}
	err = viper.Unmarshal(ret)
	if err != nil {
		return nil, errors.Wrap(err, "unmarshal config file failed")
	}
	if err := ret.Validate(); err != nil {
		return nil, err
	}
	return ret, nil
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

// Backend selects a single etcd revision domain, preserving Kubernetes storage semantics.
type Backend struct {
	Endpoint string `json:"endpoint"`
	TLS      TLS    `json:"tls"`
}

// TLS describes transport security. ClientCertAuth applies to the listener only;
// ServerName applies to backend certificate verification only.
type TLS struct {
	Enabled        bool   `json:"enabled"`
	CertFile       string `json:"certFile"`
	KeyFile        string `json:"keyFile"`
	CAFile         string `json:"caFile"`
	ServerName     string `json:"serverName"`
	ClientCertAuth bool   `json:"clientCertAuth"`
}

func (t TLS) IsEnabled() bool {
	return t.Enabled || t.CertFile != "" || t.KeyFile != "" || t.CAFile != "" || t.ServerName != "" || t.ClientCertAuth
}

func (t TLS) Validate(frontend bool) error {
	if (t.CertFile == "") != (t.KeyFile == "") {
		return fmt.Errorf("TLS certFile and keyFile must be provided together")
	}
	if frontend && t.IsEnabled() && t.CertFile == "" {
		return fmt.Errorf("listener TLS requires certFile and keyFile")
	}
	if frontend && t.ClientCertAuth && t.CAFile == "" {
		return fmt.Errorf("clientCertAuth requires caFile")
	}
	if frontend && t.ServerName != "" {
		return fmt.Errorf("serverName is only supported for backend TLS")
	}
	if !frontend && t.ClientCertAuth {
		return fmt.Errorf("clientCertAuth is only supported for listener TLS")
	}
	return nil
}

func (c *Configurations) Validate() error {
	if err := c.TLS.Validate(true); err != nil {
		return err
	}
	if c.Backend != nil {
		if c.Backend.Endpoint == "" {
			return fmt.Errorf("backend endpoint is required")
		}
		if len(c.Shards) != 0 {
			return fmt.Errorf("backend and shards are mutually exclusive")
		}
		return c.Backend.TLS.Validate(false)
	}
	if len(c.Shards) == 0 {
		return fmt.Errorf("configure backend or at least one shard")
	}
	var previousEnd []byte
	for i, s := range c.Shards {
		if s.Address == "" {
			return fmt.Errorf("shard %d address is required", i)
		}
		start, end := s.StartBytes, s.EndBytes
		if s.Start != "" {
			start = []byte(s.Start)
		}
		if s.End != "" {
			end = []byte(s.End)
		}
		if i == 0 {
			start = nil
		}
		if i > 0 && !bytes.Equal(start, previousEnd) {
			return fmt.Errorf("shard %d must start at previous shard end", i)
		}
		if i < len(c.Shards)-1 && (len(end) == 0 || bytes.Equal(end, []byte{0}) || bytes.Compare(start, end) >= 0) {
			return fmt.Errorf("shard %d has invalid end boundary", i)
		}
		previousEnd = end
	}
	return nil
}
