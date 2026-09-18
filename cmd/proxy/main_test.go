package main

import (
	"bytes"
	"context"
	"math"
	"net"
	"testing"
	"time"

	"github.com/sharding-db/etcd-sharding-proxy/pkg/config"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestBuildBackends(t *testing.T) {
	for _, tc := range []struct {
		name               string
		c                  config.Configurations
		valid, maintenance bool
	}{
		{"backend", config.Configurations{Backend: &config.Backend{Endpoint: "localhost:2379"}}, true, true},
		{"legacy", config.Configurations{Shards: []config.Shard{{Address: "localhost:2379"}}}, true, false},
		{"invalid", config.Configurations{}, false, false},
		{"bad TLS", config.Configurations{Backend: &config.Backend{Endpoint: "localhost:2379", TLS: config.TLS{CAFile: "missing"}}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backends, closeBackend, err := buildBackends(&tc.c)
			defer closeBackend()
			if (err == nil) != tc.valid {
				t.Fatalf("build error=%v", err)
			}
			if err == nil {
				if backends.KV == nil || backends.Watch == nil || backends.Lease == nil {
					t.Fatal("missing core services")
				}
				if (backends.Maintenance != nil) != tc.maintenance {
					t.Fatal("unexpected maintenance registration")
				}
			}
		})
	}
}

type largeRangeBackend struct {
	pb.UnimplementedKVServer
	pb.UnimplementedWatchServer
	value []byte
}

func (b *largeRangeBackend) Range(context.Context, *pb.RangeRequest) (*pb.RangeResponse, error) {
	return &pb.RangeResponse{Kvs: []*mvccpb.KeyValue{{Key: []byte("/registry/pods/test"), Value: b.value}}, Count: 1}, nil
}

func TestBuiltBackendForwardsLargeRange(t *testing.T) {
	// Kubernetes LIST aggregates objects and routinely exceeds gRPC's default
	// 4 MiB receive limit even though each individual write fits etcd's limit.
	value := bytes.Repeat([]byte("x"), 5*1024*1024)
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstream := grpc.NewServer()
	pb.RegisterKVServer(upstream, &largeRangeBackend{value: value})
	pb.RegisterWatchServer(upstream, &largeRangeBackend{value: value})
	go upstream.Serve(upstreamListener)
	defer upstream.Stop()
	backends, closeBackend, err := buildBackends(&config.Configurations{Backend: &config.Backend{Endpoint: upstreamListener.Addr().String()}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := grpc.NewServer()
	pb.RegisterKVServer(proxy, backends.KV)
	pb.RegisterWatchServer(proxy, backends.Watch)
	go proxy.Serve(proxyListener)
	defer proxy.Stop()
	conn, err := grpc.Dial(proxyListener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := pb.NewKVClient(conn).Range(ctx, &pb.RangeRequest{Key: []byte("/registry/pods/"), RangeEnd: []byte("/registry/pods0")})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Kvs) != 1 || !bytes.Equal(response.Kvs[0].Value, value) {
		t.Fatal("large range response was altered")
	}
	watch, err := pb.NewWatchClient(conn).Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := watch.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: &pb.WatchCreateRequest{Key: []byte("/registry/pods/"), RangeEnd: []byte("/registry/pods0")}}}); err != nil {
		t.Fatal(err)
	}
	batch, err := watch.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 5 {
		t.Fatalf("events=%d", len(batch.Events))
	}
	for _, event := range batch.Events {
		if !bytes.Equal(event.Kv.Value, value[:1024*1024]) {
			t.Fatal("watch value altered")
		}
	}

}

func (b *largeRangeBackend) Watch(stream pb.Watch_WatchServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	// Five legal-sized objects can exceed the transport default in one batch.
	events := make([]*mvccpb.Event, 5)
	for i := range events {
		events[i] = &mvccpb.Event{Kv: &mvccpb.KeyValue{Key: []byte("/registry/pods/test"), Value: b.value[:1024*1024]}}
	}
	return stream.Send(&pb.WatchResponse{Events: events})
}

// Constructor handshake and empty range prove coordinated mode is actually
// wired to the metadata backend, with all public services registered.
type coordinatedWiringBackend struct {
	pb.UnimplementedKVServer
}

func (*coordinatedWiringBackend) Txn(context.Context, *pb.TxnRequest) (*pb.TxnResponse, error) {
	return &pb.TxnResponse{Succeeded: true}, nil
}
func (*coordinatedWiringBackend) Range(context.Context, *pb.RangeRequest) (*pb.RangeResponse, error) {
	return &pb.RangeResponse{Header: &pb.ResponseHeader{Revision: 42}}, nil
}

func TestBuildCoordinatedBackends(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstream := grpc.NewServer()
	pb.RegisterKVServer(upstream, &coordinatedWiringBackend{})
	go upstream.Serve(listener)
	defer upstream.Stop()
	conf := config.Configurations{Coordinator: &config.Backend{Endpoint: listener.Addr().String()}, Shards: []config.Shard{
		{Address: "127.0.0.1:1", Start: "ignored", End: "m"},
		{Address: "127.0.0.1:2", Start: "m", End: "ignored"},
	}}
	bes, closeBackend, err := buildBackends(&conf)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	if bes.KV == nil || bes.Watch == nil || bes.Lease == nil || bes.Maintenance == nil {
		t.Fatal("missing services")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := bes.KV.Range(ctx, &pb.RangeRequest{Key: []byte("key")})
	if err != nil || r.Header.Revision != 42 {
		t.Fatalf("response=%v err=%v", r, err)
	}
	closeBackend()
	if _, err := bes.KV.Range(ctx, &pb.RangeRequest{Key: []byte("key")}); err == nil {
		t.Fatal("metadata connection not closed")
	}
	for _, target := range []string{"coordinator", "shard", "identity"} {
		t.Run(target, func(t *testing.T) {
			c := config.Configurations{Coordinator: &config.Backend{Endpoint: listener.Addr().String()}, Shards: []config.Shard{{Address: "127.0.0.1:1"}}}
			switch target {
			case "coordinator":
				c.Coordinator.TLS = config.TLS{CAFile: "missing-ca"}
			case "shard":
				c.Shards[0].TLS = config.TLS{CAFile: "missing-ca"}
			case "identity":
				c.Shards[0].Address = c.Coordinator.Endpoint
			}
			_, closeFailed, err := buildBackends(&c)
			defer closeFailed()
			if err == nil {
				t.Fatal("invalid coordinated configuration accepted")
			}
		})
	}
}
