package server

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type leaseTestConfigs struct {
	ShardingConfigs
	clients []ShardClient
}

func (c leaseTestConfigs) GetAllShardClis() []ShardClient { return c.clients }

type leaseTestClient struct {
	ShardClient
	grant  func(*pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error)
	revoke func(*pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error)
	ttl    func(*pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error)
	list   func() (*pb.LeaseLeasesResponse, error)
	keep   func(context.Context) (pb.Lease_LeaseKeepAliveClient, error)
}

func (c leaseTestClient) LeaseGrant(_ context.Context, r *pb.LeaseGrantRequest, _ ...grpc.CallOption) (*pb.LeaseGrantResponse, error) {
	return c.grant(r)
}
func (c leaseTestClient) LeaseRevoke(_ context.Context, r *pb.LeaseRevokeRequest, _ ...grpc.CallOption) (*pb.LeaseRevokeResponse, error) {
	return c.revoke(r)
}
func (c leaseTestClient) LeaseTimeToLive(_ context.Context, r *pb.LeaseTimeToLiveRequest, _ ...grpc.CallOption) (*pb.LeaseTimeToLiveResponse, error) {
	return c.ttl(r)
}
func (c leaseTestClient) LeaseLeases(context.Context, *pb.LeaseLeasesRequest, ...grpc.CallOption) (*pb.LeaseLeasesResponse, error) {
	return c.list()
}
func (c leaseTestClient) LeaseKeepAlive(ctx context.Context, _ ...grpc.CallOption) (pb.Lease_LeaseKeepAliveClient, error) {
	return c.keep(ctx)
}

func TestLeaseUnary(t *testing.T) {
	ctx := context.Background()
	t.Run("grant and revoke all shards", func(t *testing.T) {
		var ids []int64
		var revoked []int64
		client := leaseTestClient{
			grant: func(r *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
				ids = append(ids, r.ID)
				return &pb.LeaseGrantResponse{ID: r.ID, TTL: r.TTL}, nil
			},
			revoke: func(r *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
				revoked = append(revoked, r.ID)
				return &pb.LeaseRevokeResponse{}, nil
			},
		}
		proxy := NewLeaseProxy(leaseTestConfigs{clients: []ShardClient{client, client}})
		for _, id := range []int64{0, 42} {
			resp, err := proxy.LeaseGrant(ctx, &pb.LeaseGrantRequest{ID: id, TTL: 60})
			require.NoError(t, err)
			require.NotZero(t, resp.ID)
			require.Equal(t, ids[len(ids)-2], ids[len(ids)-1])
			if id != 0 {
				require.Equal(t, id, resp.ID)
			}
			_, err = proxy.LeaseRevoke(ctx, &pb.LeaseRevokeRequest{ID: resp.ID})
			require.NoError(t, err)
			require.Equal(t, revoked[len(revoked)-2], revoked[len(revoked)-1])
		}
	})
	t.Run("aggregate keys and minimum TTL without mutating backend", func(t *testing.T) {
		a := &pb.LeaseTimeToLiveResponse{ID: 42, TTL: 50, GrantedTTL: 60, Keys: [][]byte{[]byte("a")}}
		b := &pb.LeaseTimeToLiveResponse{ID: 42, TTL: 40, GrantedTTL: 60, Keys: [][]byte{[]byte("z")}}
		proxy := NewLeaseProxy(leaseTestConfigs{clients: []ShardClient{
			leaseTestClient{ttl: func(r *pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error) {
				require.True(t, r.Keys)
				return a, nil
			}},
			leaseTestClient{ttl: func(*pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error) { return b, nil }},
		}})
		resp, err := proxy.LeaseTimeToLive(ctx, &pb.LeaseTimeToLiveRequest{ID: 42, Keys: true})
		require.NoError(t, err)
		require.Equal(t, [][]byte{[]byte("a"), []byte("z")}, resp.Keys)
		require.EqualValues(t, 40, resp.TTL)
		require.EqualValues(t, 60, resp.GrantedTTL)
		require.Len(t, a.Keys, 1)
		require.EqualValues(t, 50, a.TTL)
	})
	t.Run("list only first shard", func(t *testing.T) {
		expected := &pb.LeaseLeasesResponse{Leases: []*pb.LeaseStatus{{ID: 42}}}
		proxy := NewLeaseProxy(leaseTestConfigs{clients: []ShardClient{
			leaseTestClient{list: func() (*pb.LeaseLeasesResponse, error) { return expected, nil }},
			leaseTestClient{list: func() (*pb.LeaseLeasesResponse, error) { t.Fatal("second shard queried"); return nil, nil }},
		}})
		resp, err := proxy.LeaseLeases(ctx, &pb.LeaseLeasesRequest{})
		require.NoError(t, err)
		require.Equal(t, expected, resp)
	})
	t.Run("backend errors", func(t *testing.T) {
		want := status.Error(codes.Unavailable, "backend down")
		proxy := NewLeaseProxy(leaseTestConfigs{clients: []ShardClient{leaseTestClient{
			grant:  func(*pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) { return nil, want },
			revoke: func(*pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) { return nil, want },
			ttl:    func(*pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error) { return nil, want },
			list:   func() (*pb.LeaseLeasesResponse, error) { return nil, want },
		}}})
		_, err := proxy.LeaseGrant(ctx, &pb.LeaseGrantRequest{ID: 42})
		require.Equal(t, want, err)
		_, err = proxy.LeaseRevoke(ctx, &pb.LeaseRevokeRequest{ID: 42})
		require.Equal(t, want, err)
		_, err = proxy.LeaseTimeToLive(ctx, &pb.LeaseTimeToLiveRequest{ID: 42})
		require.Equal(t, want, err)
		_, err = proxy.LeaseLeases(ctx, &pb.LeaseLeasesRequest{})
		require.Equal(t, want, err)
	})
	_, err := NewLeaseProxy(leaseTestConfigs{}).LeaseLeases(ctx, &pb.LeaseLeasesRequest{})
	require.Equal(t, codes.Unavailable, status.Code(err))
}

type leaseTestStream struct {
	grpc.ClientStream
	ctx      context.Context
	id       int64
	ttl      int64
	sendErr  error
	recvErr  error
	mismatch bool
}

func (s *leaseTestStream) Send(r *pb.LeaseKeepAliveRequest) error { s.id = r.ID; return s.sendErr }
func (s *leaseTestStream) Recv() (*pb.LeaseKeepAliveResponse, error) {
	if s.recvErr != nil {
		return nil, s.recvErr
	}
	id := s.id
	if s.mismatch {
		id++
	}
	return &pb.LeaseKeepAliveResponse{ID: id, TTL: s.ttl}, nil
}

// Exercise the handler through real gRPC streams, including client half-close.
func leaseTestRPC(t *testing.T, configs ShardingConfigs) pb.LeaseClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	pb.RegisterLeaseServer(server, NewLeaseProxy(configs))
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	conn, err := grpc.DialContext(context.Background(), "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return pb.NewLeaseClient(conn)
}
func TestLeaseKeepAliveRPC(t *testing.T) {
	t.Run("one response per request and EOF", func(t *testing.T) {
		canceled := make(chan context.Context, 2)
		var clients []ShardClient
		for _, ttl := range []int64{60, 40} {
			ttl := ttl
			clients = append(clients, leaseTestClient{keep: func(ctx context.Context) (pb.Lease_LeaseKeepAliveClient, error) {
				canceled <- ctx
				return &leaseTestStream{ctx: ctx, ttl: ttl}, nil
			}})
		}
		client := leaseTestRPC(t, leaseTestConfigs{clients: clients})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := client.LeaseKeepAlive(ctx)
		require.NoError(t, err)
		for _, id := range []int64{42, 43, 42} {
			require.NoError(t, stream.Send(&pb.LeaseKeepAliveRequest{ID: id}))
			resp, err := stream.Recv()
			require.NoError(t, err)
			require.Equal(t, id, resp.ID)
			require.EqualValues(t, 40, resp.TTL)
		}
		require.NoError(t, stream.CloseSend())
		_, err = stream.Recv()
		require.Equal(t, io.EOF, err)
		for i := 0; i < 2; i++ {
			select {
			case backend := <-canceled:
				select {
				case <-backend.Done():
				case <-ctx.Done():
					t.Fatal("backend not canceled")
				}
			case <-ctx.Done():
				t.Fatal("backend not opened")
			}
		}
	})
	for _, stage := range []string{"open", "send", "receive", "backend EOF", "send EOF", "send EOF status", "mismatch", "empty", "expired"} {
		t.Run(stage, func(t *testing.T) {
			want := status.Error(codes.Unavailable, "backend failed")
			backend := &leaseTestStream{ttl: 60}
			if stage == "send" {
				backend.sendErr = want
			}
			if stage == "receive" {
				backend.recvErr = want
			}
			if stage == "send EOF" {
				backend.sendErr = io.EOF
				backend.recvErr = io.EOF
			}
			if stage == "send EOF status" {
				backend.sendErr = io.EOF
				backend.recvErr = status.Error(codes.PermissionDenied, "denied")
			}
			if stage == "backend EOF" {
				backend.recvErr = io.EOF
			}
			if stage == "mismatch" {
				backend.mismatch = true
			}
			if stage == "expired" {
				backend.ttl = 0
			}
			configs := leaseTestConfigs{clients: []ShardClient{leaseTestClient{keep: func(ctx context.Context) (pb.Lease_LeaseKeepAliveClient, error) {
				backend.ctx = ctx
				if stage == "open" {
					return nil, want
				}
				return backend, nil
			}}}}
			if stage == "empty" {
				configs.clients = nil
			}
			client := leaseTestRPC(t, configs)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := client.LeaseKeepAlive(ctx)
			require.NoError(t, err)
			err = stream.Send(&pb.LeaseKeepAliveRequest{ID: 42})
			require.True(t, err == nil || errors.Is(err, io.EOF))
			resp, err := stream.Recv()
			switch stage {
			case "expired":
				require.NoError(t, err)
				require.Zero(t, resp.TTL)
			case "send EOF status":
				require.Equal(t, codes.PermissionDenied, status.Code(err))
			case "mismatch":
				require.Equal(t, codes.Internal, status.Code(err))
			default:
				require.Equal(t, codes.Unavailable, status.Code(err))
			}
		})
	}
	t.Run("client cancellation", func(t *testing.T) {
		client := leaseTestRPC(t, leaseTestConfigs{clients: []ShardClient{leaseTestClient{}}})
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := client.LeaseKeepAlive(ctx)
		require.NoError(t, err)
		cancel()
		_, err = stream.Recv()
		require.Equal(t, codes.Canceled, status.Code(err))
	})
}
