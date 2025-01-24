package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sharding-db/etcd-sharding-proxy/pkg/config"
	"github.com/sharding-db/etcd-sharding-proxy/pkg/server"
)

func main() {
	var addr string
	var port int
	var configPath string
	flag.StringVar(&addr, "addr", "", "proxy listen address")
	flag.IntVar(&port, "port", 2379, "proxy listen port")
	flag.StringVar(&configPath, "config", "./config.yaml", "proxy config file path")
	flag.Parse()

	fmt.Println("Etcd Sharding Proxy starting...")

	conf, err := config.NewConfigurationsFromFile(configPath)
	if err != nil {
		exitWithErr(err, "load config file")
	}

	var shards = make([]server.Shard, len(conf.Shards))
	for i, shard := range conf.Shards {
		shards[i], err = server.NewShardImpl(i, len(shards), shard)
		if err != nil {
			exitWithErr(err, fmt.Sprintf("create shard[%d]", i))
		}
	}
	shardingConfigs := server.NewDefaultShardingConfigs(shards)

	proxykv := server.NewKVProxy(server.NewDefaultGroupRunnerFactory(), shardingConfigs, new(server.DefaultResponseFilter))
	proxywatch := server.NewWatchProxy(shardingConfigs)
	proxylease := server.NewLeaseProxy(shardingConfigs)
	bes := server.BackendServers{
		KV:    proxykv,
		Watch: proxywatch,
		Lease: proxylease,
	}
	server, err := server.NewGrpcServer(bes)
	if err != nil {
		exitWithErr(err, "create grpc server")
	}
	fmt.Printf("grpc server serves on %s:%d\n", addr, port)
	err = server.Serve(addr, port)
	if err != nil {
		exitWithErr(err, "grpc server serve")
	}
}

func exitWithErr(err error, stage string) {
	fmt.Println(stage, " failed: ", err, stage)
	os.Exit(1)
}
