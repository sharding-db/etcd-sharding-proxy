//go:build integration

package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

func startBackendIntegration(t *testing.T) (*grpc.ClientConn, *grpc.ClientConn) {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required")
	}
	freeAddr := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		a := l.Addr().String()
		require.NoError(t, l.Close())
		return a
	}
	addr, peer := freeAddr(), freeAddr()
	dir := t.TempDir()
	log, err := os.Create(filepath.Join(dir, "etcd.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.Command(filepath.Join(assets, "etcd"), "--name=integration", "--data-dir="+filepath.Join(dir, "data"), "--listen-client-urls=http://"+addr, "--advertise-client-urls=http://"+addr, "--listen-peer-urls=http://"+peer, "--initial-advertise-peer-urls=http://"+peer, "--initial-cluster=integration=http://"+peer, "--initial-cluster-token=proxy-integration", "--logger=zap", "--log-level=error")
	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	backend, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	// Dial readiness alone precedes leader election. A linearizable read verifies it.
	for {
		_, err = pb.NewKVClient(backend).Range(ctx, &pb.RangeRequest{Key: []byte("ready")})
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("etcd readiness: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	proxy := NewBackendProxy(backend)
	s := grpc.NewServer()
	pb.RegisterKVServer(s, proxy)
	pb.RegisterWatchServer(s, proxy)
	pb.RegisterLeaseServer(s, proxy)
	pb.RegisterMaintenanceServer(s, proxy)
	lis := bufconn.Listen(1024 * 1024)
	go s.Serve(lis)
	t.Cleanup(func() { s.Stop(); _ = lis.Close() })
	conn, err := grpc.DialContext(ctx, "integration-proxy", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn, backend
}

func TestBackendProxyRealEtcd(t *testing.T) {
	conn, backend := startBackendIntegration(t)
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("hasleader", "true")), 20*time.Second)
	defer cancel()
	kv, lease := pb.NewKVClient(conn), pb.NewLeaseClient(conn)
	put := func(key, value string) *pb.PutResponse {
		r, err := kv.Put(ctx, &pb.PutRequest{Key: []byte(key), Value: []byte(value)})
		require.NoError(t, err)
		return r
	}
	t.Run("maintenance status", func(t *testing.T) {
		got, err := pb.NewMaintenanceClient(conn).Status(ctx, &pb.StatusRequest{})
		require.NoError(t, err)
		want, err := pb.NewMaintenanceClient(backend).Status(ctx, &pb.StatusRequest{})
		require.NoError(t, err)
		require.Equal(t, want.Version, got.Version)
		require.True(t, strings.HasPrefix(got.Version, "3."))
		require.Equal(t, want.Header.ClusterId, got.Header.ClusterId)
		require.NotZero(t, got.Leader)
	})
	t.Run("watch replay prevkv progress cancel and compaction", func(t *testing.T) {
		first := put("/registry/watch/key", "before")
		second := put("/registry/watch/key", "after")
		watch, err := pb.NewWatchClient(conn).Watch(ctx)
		require.NoError(t, err)
		require.NoError(t, watch.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: &pb.WatchCreateRequest{Key: []byte("/registry/watch/key"), StartRevision: first.Header.Revision + 1, PrevKv: true, WatchId: 17}}}))
		created, err := watch.Recv()
		require.NoError(t, err)
		require.True(t, created.Created)
		require.EqualValues(t, 17, created.WatchId)
		event, err := watch.Recv()
		require.NoError(t, err)
		require.Len(t, event.Events, 1)
		require.Equal(t, []byte("before"), event.Events[0].PrevKv.Value)
		require.Equal(t, []byte("after"), event.Events[0].Kv.Value)
		require.Equal(t, second.Header.Revision, event.Events[0].Kv.ModRevision)
		require.NoError(t, watch.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_ProgressRequest{ProgressRequest: &pb.WatchProgressRequest{}}}))
		progress, err := watch.Recv()
		require.NoError(t, err)
		require.Empty(t, progress.Events)
		require.GreaterOrEqual(t, progress.Header.Revision, second.Header.Revision)
		require.NoError(t, watch.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CancelRequest{CancelRequest: &pb.WatchCancelRequest{WatchId: 17}}}))
		canceled, err := watch.Recv()
		require.NoError(t, err)
		require.True(t, canceled.Canceled)
		require.EqualValues(t, 17, canceled.WatchId)
		_, err = kv.Compact(ctx, &pb.CompactionRequest{Revision: second.Header.Revision, Physical: true})
		require.NoError(t, err)
		_, err = kv.Range(ctx, &pb.RangeRequest{Key: []byte("/registry/watch/key"), Revision: first.Header.Revision})
		require.ErrorContains(t, err, "required revision has been compacted")
		require.NoError(t, watch.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: &pb.WatchCreateRequest{Key: []byte("/registry/watch/key"), StartRevision: first.Header.Revision, WatchId: 18}}}))
		for {
			r, err := watch.Recv()
			require.NoError(t, err)
			if r.Canceled {
				require.EqualValues(t, 18, r.WatchId)
				require.Equal(t, second.Header.Revision, r.CompactRevision)
				break
			}
		}
		require.NoError(t, watch.CloseSend())
	})
	t.Run("lease txn keepalive ttl and revoke delete", func(t *testing.T) {
		grant, err := lease.LeaseGrant(ctx, &pb.LeaseGrantRequest{TTL: 30})
		require.NoError(t, err)
		key := []byte("/registry/events/leased")
		txn, err := kv.Txn(ctx, &pb.TxnRequest{Compare: []*pb.Compare{{Key: key, Target: pb.Compare_VERSION, Result: pb.Compare_EQUAL, TargetUnion: &pb.Compare_Version{Version: 0}}}, Success: []*pb.RequestOp{{Request: &pb.RequestOp_RequestPut{RequestPut: &pb.PutRequest{Key: key, Value: []byte("event"), Lease: grant.ID}}}}})
		require.NoError(t, err)
		require.True(t, txn.Succeeded)
		kaCtx, kaCancel := context.WithCancel(ctx)
		defer kaCancel()
		keep, err := lease.LeaseKeepAlive(kaCtx)
		require.NoError(t, err)
		require.NoError(t, keep.Send(&pb.LeaseKeepAliveRequest{ID: grant.ID}))
		renewed, err := keep.Recv()
		require.NoError(t, err)
		require.Equal(t, grant.ID, renewed.ID)
		require.Greater(t, renewed.TTL, int64(0))
		kaCancel()
		ttl, err := lease.LeaseTimeToLive(ctx, &pb.LeaseTimeToLiveRequest{ID: grant.ID, Keys: true})
		require.NoError(t, err)
		require.Equal(t, [][]byte{key}, ttl.Keys)
		wc, wcancel := context.WithCancel(ctx)
		defer wcancel()
		watch, err := pb.NewWatchClient(conn).Watch(wc)
		require.NoError(t, err)
		require.NoError(t, watch.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: &pb.WatchCreateRequest{Key: key, StartRevision: txn.Header.Revision + 1, PrevKv: true}}}))
		created, err := watch.Recv()
		require.NoError(t, err)
		require.True(t, created.Created)
		_, err = lease.LeaseRevoke(ctx, &pb.LeaseRevokeRequest{ID: grant.ID})
		require.NoError(t, err)
		deleted, err := watch.Recv()
		require.NoError(t, err)
		require.Len(t, deleted.Events, 1)
		require.Equal(t, mvccpb.DELETE, deleted.Events[0].Type)
		require.Equal(t, []byte("event"), deleted.Events[0].PrevKv.Value)
		got, err := kv.Range(ctx, &pb.RangeRequest{Key: key})
		require.NoError(t, err)
		require.Empty(t, got.Kvs)
	})
	t.Log(fmt.Sprintf("real etcd integration complete through proxy %s", conn.Target()))
}
