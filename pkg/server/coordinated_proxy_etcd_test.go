//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func coordinatedTestClient(t *testing.T, ctx context.Context, coordinator *grpc.ClientConn, shards []CoordinatedShard) *grpc.ClientConn {
	t.Helper()
	proxy, err := NewCoordinatedProxy(ctx, coordinator, shards)
	require.NoError(t, err)
	server := grpc.NewServer()
	pb.RegisterKVServer(server, proxy)
	pb.RegisterWatchServer(server, proxy)
	pb.RegisterLeaseServer(server, proxy)
	pb.RegisterMaintenanceServer(server, proxy)
	lis := bufconn.Listen(1024 * 1024)
	go server.Serve(lis)
	t.Cleanup(func() { server.Stop(); _ = lis.Close() })
	conn, err := grpc.DialContext(ctx, "coordinated-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func coordinatedPutOp(key, value string) *pb.RequestOp {
	return &pb.RequestOp{Request: &pb.RequestOp_RequestPut{RequestPut: &pb.PutRequest{Key: []byte(key), Value: []byte(value)}}}
}

func TestCoordinatedProxyRealEtcd(t *testing.T) {
	_, coordinator := startBackendIntegration(t)
	_, left := startBackendIntegration(t)
	_, right := startBackendIntegration(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	shards := []CoordinatedShard{{End: []byte("m"), Endpoint: left.Target(), Conn: left}, {Start: []byte("m"), Endpoint: right.Target(), Conn: right}}
	conn := coordinatedTestClient(t, ctx, coordinator, shards)
	kv := pb.NewKVClient(conn)
	put := func(key, value string) *pb.PutResponse {
		r, e := kv.Put(ctx, &pb.PutRequest{Key: []byte(key), Value: []byte(value)})
		require.NoError(t, e)
		return r
	}
	read := func(key string, rev int64) *pb.RangeResponse {
		r, e := kv.Range(ctx, &pb.RangeRequest{Key: []byte(key), Revision: rev})
		require.NoError(t, e)
		return r
	}
	t.Run("global revisions native CAS and physical placement", func(t *testing.T) {
		a := put("a", "left-value")
		z := put("z", "right-value")
		require.Greater(t, z.Header.Revision, a.Header.Revision)
		require.Equal(t, a.Header.Revision, read("a", 0).Kvs[0].CreateRevision)
		for _, target := range []pb.Compare_CompareTarget{pb.Compare_CREATE, pb.Compare_MOD, pb.Compare_VERSION} {
			before := read("a", 0).Kvs[0]
			c := &pb.Compare{Key: []byte("a"), Target: target, Result: pb.Compare_EQUAL}
			switch target {
			case pb.Compare_CREATE:
				c.TargetUnion = &pb.Compare_CreateRevision{CreateRevision: before.CreateRevision}
			case pb.Compare_MOD:
				c.TargetUnion = &pb.Compare_ModRevision{ModRevision: before.ModRevision}
			case pb.Compare_VERSION:
				c.TargetUnion = &pb.Compare_Version{Version: before.Version}
			}
			tr, e := kv.Txn(ctx, &pb.TxnRequest{Compare: []*pb.Compare{c}, Success: []*pb.RequestOp{coordinatedPutOp("a", "updated")}})
			require.NoError(t, e)
			require.True(t, tr.Succeeded)
			after := read("a", 0).Kvs[0]
			require.Equal(t, before.Version+1, after.Version)
			require.Equal(t, before.CreateRevision, after.CreateRevision)
			require.Equal(t, tr.Header.Revision, after.ModRevision)
		}
		for _, tc := range []struct {
			key     string
			shard   int
			backend *grpc.ClientConn
		}{{"a", 0, left}, {"z", 1, right}} {
			r, e := pb.NewKVClient(coordinator).Range(ctx, &pb.RangeRequest{Key: []byte(CoordinatedMetadataPrefix + tc.key)})
			require.NoError(t, e)
			require.Len(t, r.Kvs, 1)
			var ref struct {
				Shard int    `json:"shard"`
				Key   string `json:"key"`
			}
			require.NoError(t, json.Unmarshal(r.Kvs[0].Value, &ref))
			require.Equal(t, tc.shard, ref.Shard)
			require.Contains(t, ref.Key, CoordinatedBlobPrefix)
			blob, e := pb.NewKVClient(tc.backend).Range(ctx, &pb.RangeRequest{Key: []byte(ref.Key)})
			require.NoError(t, e)
			require.Len(t, blob.Kvs, 1)
			require.Equal(t, read(tc.key, 0).Kvs[0].Value, blob.Kvs[0].Value)
		}
	})
	t.Run("historical list and stable pagination", func(t *testing.T) {
		put("b", "old-b")
		snapshot := put("y", "old-y").Header.Revision
		page, e := kv.Range(ctx, &pb.RangeRequest{Key: []byte("a"), RangeEnd: []byte("zz"), Limit: 2, Revision: snapshot})
		require.NoError(t, e)
		require.True(t, page.More)
		require.Len(t, page.Kvs, 2)
		put("b", "new-b")
		put("x", "new-x")
		put("y", "new-y")
		next := append(append([]byte(nil), page.Kvs[1].Key...), 0)
		rest, e := kv.Range(ctx, &pb.RangeRequest{Key: next, RangeEnd: []byte("zz"), Revision: snapshot})
		require.NoError(t, e)
		all := append(page.Kvs, rest.Kvs...)
		require.Len(t, all, 4)
		require.Equal(t, []byte("old-b"), all[1].Value)
		require.Equal(t, []byte("old-y"), all[2].Value)
		require.Equal(t, []byte("old-b"), read("b", snapshot).Kvs[0].Value)
	})
	t.Run("cross shard replay prevkv progress and lease expiration", func(t *testing.T) {
		first := put("c-watch", "before")
		second := put("z-watch", "remote")
		third := put("c-watch", "after")
		w, e := pb.NewWatchClient(conn).Watch(ctx)
		require.NoError(t, e)
		require.NoError(t, w.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: &pb.WatchCreateRequest{Key: []byte("a"), RangeEnd: []byte("zz"), StartRevision: first.Header.Revision, PrevKv: true, WatchId: 77}}}))
		created, e := w.Recv()
		require.NoError(t, e)
		require.True(t, created.Created)
		var events []*mvccpb.Event
		for len(events) < 3 {
			r, e := w.Recv()
			require.NoError(t, e)
			events = append(events, r.Events...)
		}
		require.Len(t, events, 3)
		require.Equal(t, first.Header.Revision, events[0].Kv.ModRevision)
		require.Equal(t, second.Header.Revision, events[1].Kv.ModRevision)
		require.Equal(t, third.Header.Revision, events[2].Kv.ModRevision)
		require.Equal(t, []byte("before"), events[2].PrevKv.Value)
		require.NoError(t, w.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_ProgressRequest{ProgressRequest: &pb.WatchProgressRequest{}}}))
		progress, e := w.Recv()
		require.NoError(t, e)
		require.Empty(t, progress.Events)
		require.GreaterOrEqual(t, progress.Header.Revision, third.Header.Revision)
		grant, e := pb.NewLeaseClient(conn).LeaseGrant(ctx, &pb.LeaseGrantRequest{TTL: 1})
		require.NoError(t, e)
		_, e = kv.Put(ctx, &pb.PutRequest{Key: []byte("z-ttl"), Value: []byte("temporary"), Lease: grant.ID})
		require.NoError(t, e)
		for {
			r, e := w.Recv()
			require.NoError(t, e)
			found := false
			for _, event := range r.Events {
				if event.Type == mvccpb.DELETE && string(event.Kv.Key) == "z-ttl" {
					require.Equal(t, []byte("temporary"), event.PrevKv.Value)
					found = true
				}
			}
			if found {
				break
			}
		}
		require.Empty(t, read("z-ttl", 0).Kvs)
		require.NoError(t, w.CloseSend())
	})
	t.Run("cross shard mutations rejected atomically", func(t *testing.T) {
		put("d-reject", "left")
		put("z-reject", "right")
		_, e := kv.Txn(ctx, &pb.TxnRequest{Success: []*pb.RequestOp{coordinatedPutOp("d-reject", "bad"), coordinatedPutOp("z-reject", "bad")}})
		require.Error(t, e)
		_, e = kv.DeleteRange(ctx, &pb.DeleteRangeRequest{Key: []byte("d-reject"), RangeEnd: []byte("zz")})
		require.Error(t, e)
		require.Equal(t, []byte("left"), read("d-reject", 0).Kvs[0].Value)
		require.Equal(t, []byte("right"), read("z-reject", 0).Kvs[0].Value)
		tr, e := kv.Txn(ctx, &pb.TxnRequest{Compare: []*pb.Compare{{Key: []byte("d-reject"), Target: pb.Compare_VERSION, Result: pb.Compare_EQUAL, TargetUnion: &pb.Compare_Version{Version: 0}}}, Success: []*pb.RequestOp{coordinatedPutOp("d-reject", "failed-cas")}})
		require.NoError(t, e)
		require.False(t, tr.Succeeded)
		require.Equal(t, []byte("left"), read("d-reject", 0).Kvs[0].Value)
	})
	t.Run("multiple instances serialize CAS and restart preserves state", func(t *testing.T) {
		other := pb.NewKVClient(coordinatedTestClient(t, ctx, coordinator, shards))
		var winners int32
		var wg sync.WaitGroup
		errs := make(chan error, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				client := kv
				if i%2 == 1 {
					client = other
				}
				r, e := client.Txn(ctx, &pb.TxnRequest{Compare: []*pb.Compare{{Key: []byte("e-race"), Target: pb.Compare_VERSION, Result: pb.Compare_EQUAL, TargetUnion: &pb.Compare_Version{Version: 0}}}, Success: []*pb.RequestOp{coordinatedPutOp("e-race", "winner")}})
				if e != nil {
					errs <- e
					return
				}
				if r.Succeeded {
					atomic.AddInt32(&winners, 1)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			require.NoError(t, e)
		}
		require.EqualValues(t, 1, winners)
		fresh := pb.NewKVClient(coordinatedTestClient(t, ctx, coordinator, shards))
		r, e := fresh.Range(ctx, &pb.RangeRequest{Key: []byte("e-race")})
		require.NoError(t, e)
		require.Len(t, r.Kvs, 1)
		require.Equal(t, []byte("winner"), r.Kvs[0].Value)
		require.EqualValues(t, 1, r.Kvs[0].Version)
		changed := append([]CoordinatedShard(nil), shards...)
		changed[0].End = []byte("n")
		changed[1].Start = []byte("n")
		_, e = NewCoordinatedProxy(ctx, coordinator, changed)
		require.Error(t, e)
	})
	t.Run("lost data reply leaves invisible orphan and lost commit reply survives restart", func(t *testing.T) {
		var drop atomic.Bool
		intercepted, e := grpc.DialContext(ctx, left.Target(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			e := invoke(ctx, method, req, reply, cc, opts...)
			if e == nil && method == "/etcdserverpb.KV/Put" && drop.CompareAndSwap(true, false) {
				return status.Error(codes.Unavailable, "injected lost successful blob response")
			}
			return e
		}))
		require.NoError(t, e)
		defer intercepted.Close()
		modified := append([]CoordinatedShard(nil), shards...)
		modified[0].Conn = intercepted
		client := pb.NewKVClient(coordinatedTestClient(t, ctx, coordinator, modified))
		blobsBefore, e := pb.NewKVClient(left).Range(ctx, &pb.RangeRequest{Key: []byte(CoordinatedBlobPrefix), RangeEnd: []byte(CoordinatedBlobPrefix + "\xff"), CountOnly: true})
		require.NoError(t, e)
		drop.Store(true)
		_, e = client.Put(ctx, &pb.PutRequest{Key: []byte("f-orphan"), Value: []byte("invisible")})
		require.Error(t, e)
		require.Empty(t, read("f-orphan", 0).Kvs)
		require.False(t, drop.Load(), "fault must reach the blob write")
		blobsAfter, e := pb.NewKVClient(left).Range(ctx, &pb.RangeRequest{Key: []byte(CoordinatedBlobPrefix), RangeEnd: []byte(CoordinatedBlobPrefix + "\xff"), CountOnly: true})
		require.NoError(t, e)
		require.Equal(t, blobsBefore.Count+1, blobsAfter.Count, "successful data write must leave a real unreferenced blob")
		recovered := pb.NewKVClient(coordinatedTestClient(t, ctx, coordinator, shards))
		invisible, e := recovered.Range(ctx, &pb.RangeRequest{Key: []byte("f-orphan")})
		require.NoError(t, e)
		require.Empty(t, invisible.Kvs, "restart must not expose an uncommitted blob")
		var commitDrop atomic.Bool
		coordIntercept, e := grpc.DialContext(ctx, coordinator.Target(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			e := invoke(ctx, method, req, reply, cc, opts...)
			if e == nil && method == "/etcdserverpb.KV/Put" && commitDrop.CompareAndSwap(true, false) {
				return status.Error(codes.Unavailable, "injected lost successful commit response")
			}
			return e
		}))
		require.NoError(t, e)
		defer coordIntercept.Close()
		client = pb.NewKVClient(coordinatedTestClient(t, ctx, coordIntercept, shards))
		commitDrop.Store(true)
		_, e = client.Put(ctx, &pb.PutRequest{Key: []byte("g-commit"), Value: []byte("durable")})
		require.Error(t, e)
		require.False(t, commitDrop.Load())
		fresh := pb.NewKVClient(coordinatedTestClient(t, ctx, coordinator, shards))
		r, e := fresh.Range(ctx, &pb.RangeRequest{Key: []byte("g-commit")})
		require.NoError(t, e)
		require.Len(t, r.Kvs, 1)
		require.Equal(t, []byte("durable"), r.Kvs[0].Value)
	})
	t.Run("unavailable data shard returns errors and never publishes metadata", func(t *testing.T) {
		var unavailable atomic.Bool
		intercepted, e := grpc.DialContext(ctx, right.Target(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			if unavailable.Load() {
				return status.Error(codes.Unavailable, "injected unavailable shard")
			}
			return invoke(ctx, method, req, reply, cc, opts...)
		}))
		require.NoError(t, e)
		defer intercepted.Close()
		modified := append([]CoordinatedShard(nil), shards...)
		modified[1].Conn = intercepted
		client := pb.NewKVClient(coordinatedTestClient(t, ctx, coordinator, modified))
		_, e = client.Put(ctx, &pb.PutRequest{Key: []byte("z-unavailable-existing"), Value: []byte("persisted")})
		require.NoError(t, e)
		unavailable.Store(true)
		_, e = client.Put(ctx, &pb.PutRequest{Key: []byte("z-unavailable-new"), Value: []byte("unpublished")})
		require.Error(t, e)
		require.Empty(t, read("z-unavailable-new", 0).Kvs)
		_, e = client.Range(ctx, &pb.RangeRequest{Key: []byte("z-unavailable-existing")})
		require.Error(t, e, "an inaccessible committed value must not be represented as absent")
		unavailable.Store(false)
		r, e := client.Range(ctx, &pb.RangeRequest{Key: []byte("z-unavailable-existing")})
		require.NoError(t, e)
		require.Len(t, r.Kvs, 1)
		require.Equal(t, []byte("persisted"), r.Kvs[0].Value)
	})
	t.Run("compaction rejects stale history and watch", func(t *testing.T) {
		first := put("h-compact", "old")
		second := put("h-compact", "new")
		_, e := kv.Compact(ctx, &pb.CompactionRequest{Revision: second.Header.Revision, Physical: true})
		require.NoError(t, e)
		_, e = kv.Range(ctx, &pb.RangeRequest{Key: []byte("h-compact"), Revision: first.Header.Revision})
		require.ErrorContains(t, e, "compacted")
		w, e := pb.NewWatchClient(conn).Watch(ctx)
		require.NoError(t, e)
		require.NoError(t, w.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: &pb.WatchCreateRequest{Key: []byte("h-compact"), StartRevision: first.Header.Revision}}}))
		for {
			r, e := w.Recv()
			require.NoError(t, e)
			if r.Canceled {
				require.GreaterOrEqual(t, r.CompactRevision, second.Header.Revision)
				break
			}
		}
	})
}
