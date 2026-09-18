package server

import (
	"fmt"
	"google.golang.org/grpc/keepalive"
	"net"
	"time"

	"github.com/pkg/errors"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
)

type GrpcServer struct {
	server *grpc.Server
}

type BackendServers struct {
	KV          pb.KVServer
	Watch       pb.WatchServer
	Lease       pb.LeaseServer
	Maintenance pb.MaintenanceServer
}

func NewGrpcServer(servers BackendServers, opts ...grpc.ServerOption) (*GrpcServer, error) {
	opts = append([]grpc.ServerOption{grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true})}, opts...)
	server := grpc.NewServer(opts...)
	ret := &GrpcServer{
		server: server,
	}
	pb.RegisterKVServer(server, servers.KV)
	pb.RegisterWatchServer(server, servers.Watch)
	pb.RegisterLeaseServer(server, servers.Lease)
	if servers.Maintenance != nil {
		pb.RegisterMaintenanceServer(server, servers.Maintenance)
	}
	return ret, nil
}

func (s *GrpcServer) Serve(addr string, port int) error {
	listen, err := net.Listen("tcp", fmt.Sprintf("%s:%d", addr, port))
	if err != nil {
		return errors.Wrap(err, "failed to listen")
	}
	err = s.server.Serve(listen)
	if err != nil {
		return errors.Wrap(err, "failed to serve")
	}
	return nil
}
