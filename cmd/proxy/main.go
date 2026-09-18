package main

import (
	"flag"
	"fmt"
	"math"
	"os"

	"github.com/sharding-db/etcd-sharding-proxy/pkg/config"
	"github.com/sharding-db/etcd-sharding-proxy/pkg/server"
	"google.golang.org/grpc"
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

	bes, closeBackend, err := buildBackends(conf)
	if err != nil {
		exitWithErr(err, "create backends")
	}
	defer closeBackend()
	var options []grpc.ServerOption
	if conf.TLS.IsEnabled() {
		creds, err := server.TransportCredentials(conf.TLS, true)
		if err != nil {
			exitWithErr(err, "load listener TLS")
		}
		options = append(options, grpc.Creds(creds))
	}
	server, err := server.NewGrpcServer(bes, options...)
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

func buildBackends(conf *config.Configurations) (server.BackendServers, func(), error) {
	emptyClose := func() {}
	if err := conf.Validate(); err != nil {
		return server.BackendServers{}, emptyClose, err
	}
	if conf.Backend != nil {
		creds, err := server.TransportCredentials(conf.Backend.TLS, false)
		if err != nil {
			return server.BackendServers{}, emptyClose, err
		}
		conn, err := grpc.Dial(conf.Backend.Endpoint, grpc.WithTransportCredentials(creds),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32)))
		if err != nil {
			return server.BackendServers{}, emptyClose, err
		}
		proxy := server.NewBackendProxy(conn)
		return server.BackendServers{KV: proxy, Watch: proxy, Lease: proxy, Maintenance: proxy}, func() { _ = conn.Close() }, nil
	}
	shards := make([]server.Shard, len(conf.Shards))
	for i, shard := range conf.Shards {
		var err error
		shards[i], err = server.NewShardImpl(i, len(shards), shard)
		if err != nil {
			return server.BackendServers{}, emptyClose, fmt.Errorf("create shard %d: %w", i, err)
		}
	}
	sharding := server.NewDefaultShardingConfigs(shards)
	return server.BackendServers{
		KV:    server.NewKVProxy(server.NewDefaultGroupRunnerFactory(), sharding, new(server.DefaultResponseFilter)),
		Watch: server.NewWatchProxy(sharding), Lease: server.NewLeaseProxy(sharding),
	}, emptyClose, nil
}
