package server

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type transparentBackend struct {
	pb.UnimplementedKVServer
	pb.UnimplementedWatchServer
	pb.UnimplementedLeaseServer
	pb.UnimplementedMaintenanceServer
	watch    func(pb.Watch_WatchServer) error
	keep     func(pb.Lease_LeaseKeepAliveServer) error
	snapshot func(*pb.SnapshotRequest, pb.Maintenance_SnapshotServer) error
}

func (b *transparentBackend) Watch(s pb.Watch_WatchServer) error                   { return b.watch(s) }
func (b *transparentBackend) LeaseKeepAlive(s pb.Lease_LeaseKeepAliveServer) error { return b.keep(s) }
func (b *transparentBackend) Snapshot(r *pb.SnapshotRequest, s pb.Maintenance_SnapshotServer) error {
	return b.snapshot(r, s)
}
func transparentConn(t *testing.T, backend *transparentBackend, intercept grpc.UnaryServerInterceptor) *grpc.ClientConn {
	t.Helper()
	connect := func(s *grpc.Server) *grpc.ClientConn {
		lis := bufconn.Listen(1024 * 1024)
		go s.Serve(lis)
		t.Cleanup(func() { s.Stop(); _ = lis.Close() })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()
		conn, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	s := grpc.NewServer(grpc.UnaryInterceptor(intercept))
	pb.RegisterKVServer(s, backend)
	pb.RegisterLeaseServer(s, backend)
	pb.RegisterWatchServer(s, backend)
	pb.RegisterMaintenanceServer(s, backend)
	proxy := NewBackendProxy(connect(s))
	front := grpc.NewServer()
	pb.RegisterKVServer(front, proxy)
	pb.RegisterLeaseServer(front, proxy)
	pb.RegisterWatchServer(front, proxy)
	pb.RegisterMaintenanceServer(front, proxy)
	return connect(front)
}
func TestBackendProxyUnary(t *testing.T) {
	expected := &pb.RangeRequest{Key: []byte("/registry/pods/"), RangeEnd: []byte("/registry/pods0"), Revision: 53, Limit: 17, KeysOnly: true}
	conn := transparentConn(t, &transparentBackend{}, func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (interface{}, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		require.Equal(t, []string{"true"}, md.Get("hasleader"))
		_ = grpc.SetHeader(ctx, metadata.Pairs("backend-header", "yes"))
		_ = grpc.SetTrailer(ctx, metadata.Pairs("backend-trailer", "yes"))
		switch info.FullMethod {
		case "/etcdserverpb.KV/Range":
			require.Equal(t, expected, req)
			return &pb.RangeResponse{Header: &pb.ResponseHeader{Revision: 53, ClusterId: 42}}, nil
		case "/etcdserverpb.KV/Compact":
			return nil, status.Error(codes.OutOfRange, "etcdserver: mvcc: required revision has been compacted")
		}
		return nil, status.Error(codes.PermissionDenied, "backend-denied")
	})
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("hasleader", "true")), 5*time.Second)
	defer cancel()
	var header, trailer metadata.MD
	resp, err := pb.NewKVClient(conn).Range(ctx, expected, grpc.Header(&header), grpc.Trailer(&trailer))
	require.NoError(t, err)
	require.EqualValues(t, 53, resp.Header.Revision)
	require.EqualValues(t, 42, resp.Header.ClusterId)
	require.Equal(t, []string{"yes"}, header.Get("backend-header"))
	require.Equal(t, []string{"yes"}, trailer.Get("backend-trailer"))
	_, err = pb.NewKVClient(conn).Compact(ctx, &pb.CompactionRequest{Revision: 3})
	require.Equal(t, codes.OutOfRange, status.Code(err))
	calls := []func() error{
		func() error { _, err := pb.NewKVClient(conn).Put(ctx, &pb.PutRequest{}); return err },
		func() error { _, err := pb.NewKVClient(conn).DeleteRange(ctx, &pb.DeleteRangeRequest{}); return err },
		func() error { _, err := pb.NewKVClient(conn).Txn(ctx, &pb.TxnRequest{}); return err },
		func() error { _, err := pb.NewLeaseClient(conn).LeaseGrant(ctx, &pb.LeaseGrantRequest{}); return err },
		func() error { _, err := pb.NewLeaseClient(conn).LeaseRevoke(ctx, &pb.LeaseRevokeRequest{}); return err },
		func() error {
			_, err := pb.NewLeaseClient(conn).LeaseTimeToLive(ctx, &pb.LeaseTimeToLiveRequest{})
			return err
		},
		func() error { _, err := pb.NewLeaseClient(conn).LeaseLeases(ctx, &pb.LeaseLeasesRequest{}); return err },
		func() error { _, err := pb.NewMaintenanceClient(conn).Alarm(ctx, &pb.AlarmRequest{}); return err },
		func() error { _, err := pb.NewMaintenanceClient(conn).Status(ctx, &pb.StatusRequest{}); return err },
		func() error {
			_, err := pb.NewMaintenanceClient(conn).Defragment(ctx, &pb.DefragmentRequest{})
			return err
		},
		func() error { _, err := pb.NewMaintenanceClient(conn).Hash(ctx, &pb.HashRequest{}); return err },
		func() error { _, err := pb.NewMaintenanceClient(conn).HashKV(ctx, &pb.HashKVRequest{}); return err },
		func() error {
			_, err := pb.NewMaintenanceClient(conn).MoveLeader(ctx, &pb.MoveLeaderRequest{})
			return err
		},
		func() error {
			_, err := pb.NewMaintenanceClient(conn).Downgrade(ctx, &pb.DowngradeRequest{})
			return err
		},
	}
	for _, call := range calls {
		require.Equal(t, codes.PermissionDenied, status.Code(call()))
	}
}
func TestBackendProxyWatchHalfCloseAndMetadata(t *testing.T) {
	conn := transparentConn(t, &transparentBackend{watch: func(s pb.Watch_WatchServer) error {
		md, _ := metadata.FromIncomingContext(s.Context())
		require.Equal(t, []string{"true"}, md.Get("hasleader"))
		req, err := s.Recv()
		require.NoError(t, err)
		require.EqualValues(t, 101, req.GetCreateRequest().StartRevision)
		_, err = s.Recv()
		require.Equal(t, io.EOF, err)
		_ = s.SendHeader(metadata.Pairs("stream-header", "yes"))
		s.SetTrailer(metadata.Pairs("stream-trailer", "yes"))
		return s.Send(&pb.WatchResponse{WatchId: 7, Created: true, Header: &pb.ResponseHeader{Revision: 101}})
	}}, nil)
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("hasleader", "true")), 5*time.Second)
	defer cancel()
	s, err := pb.NewWatchClient(conn).Watch(ctx)
	require.NoError(t, err)
	require.NoError(t, s.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: &pb.WatchCreateRequest{Key: []byte("key"), StartRevision: 101}}}))
	require.NoError(t, s.CloseSend())
	r, err := s.Recv()
	require.NoError(t, err)
	require.EqualValues(t, 7, r.WatchId)
	_, err = s.Recv()
	require.Equal(t, io.EOF, err)
	h, err := s.Header()
	require.NoError(t, err)
	require.Equal(t, []string{"yes"}, h.Get("stream-header"))
	require.Equal(t, []string{"yes"}, s.Trailer().Get("stream-trailer"))
}
func TestBackendProxyStreamErrorAndCancel(t *testing.T) {
	t.Run("backend status", func(t *testing.T) {
		conn := transparentConn(t, &transparentBackend{watch: func(s pb.Watch_WatchServer) error { return status.Error(codes.PermissionDenied, "denied") }}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s, err := pb.NewWatchClient(conn).Watch(ctx)
		require.NoError(t, err)
		_, err = s.Recv()
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})
	t.Run("cancellation reaches backend", func(t *testing.T) {
		started := make(chan struct{})
		done := make(chan struct{})
		conn := transparentConn(t, &transparentBackend{watch: func(s pb.Watch_WatchServer) error {
			close(started)
			<-s.Context().Done()
			close(done)
			return s.Context().Err()
		}}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s, err := pb.NewWatchClient(conn).Watch(ctx)
		require.NoError(t, err)
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("backend did not start")
		}
		cancel()
		_, err = s.Recv()
		require.Equal(t, codes.Canceled, status.Code(err))
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("backend did not cancel")
		}
	})
}
func TestBackendProxyKeepAliveAndSnapshot(t *testing.T) {
	conn := transparentConn(t, &transparentBackend{
		keep: func(s pb.Lease_LeaseKeepAliveServer) error {
			r, err := s.Recv()
			if err != nil {
				return err
			}
			return s.Send(&pb.LeaseKeepAliveResponse{ID: r.ID, TTL: 12})
		},
		snapshot: func(r *pb.SnapshotRequest, s pb.Maintenance_SnapshotServer) error {
			return s.Send(&pb.SnapshotResponse{Blob: []byte("snapshot"), RemainingBytes: 0})
		},
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keep, err := pb.NewLeaseClient(conn).LeaseKeepAlive(ctx)
	require.NoError(t, err)
	require.NoError(t, keep.Send(&pb.LeaseKeepAliveRequest{ID: 77}))
	r, err := keep.Recv()
	require.NoError(t, err)
	require.EqualValues(t, 77, r.ID)
	require.EqualValues(t, 12, r.TTL)
	_, err = keep.Recv()
	require.Equal(t, io.EOF, err)
	snap, err := pb.NewMaintenanceClient(conn).Snapshot(ctx, &pb.SnapshotRequest{})
	require.NoError(t, err)
	sr, err := snap.Recv()
	require.NoError(t, err)
	require.Equal(t, []byte("snapshot"), sr.Blob)
	_, err = snap.Recv()
	require.Equal(t, io.EOF, err)
}
