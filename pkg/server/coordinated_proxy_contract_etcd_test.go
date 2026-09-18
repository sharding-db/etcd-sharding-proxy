//go:build integration

package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCoordinatedProxyRealEtcdEdgeContracts(t *testing.T) {
	_, coordinator := startBackendIntegration(t)
	_, left := startBackendIntegration(t)
	_, right := startBackendIntegration(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	shards := []CoordinatedShard{{End: []byte("m"), Endpoint: left.Target(), Conn: left}, {Start: []byte("m"), Endpoint: right.Target(), Conn: right}}
	proxy, e := NewCoordinatedProxy(ctx, coordinator, shards)
	require.NoError(t, e)
	conn := coordinatedTestClient(t, ctx, coordinator, shards)
	kv, raw := pb.NewKVClient(conn), pb.NewKVClient(coordinator)
	put := func(key, value string) {
		_, e := kv.Put(ctx, &pb.PutRequest{Key: []byte(key), Value: []byte(value)})
		require.NoError(t, e)
	}
	corrupt := func(key string, value []byte) {
		_, e := raw.Put(ctx, &pb.PutRequest{Key: []byte(CoordinatedMetadataPrefix + key), Value: value})
		require.NoError(t, e)
	}
	rangeOp := func(key, end string, keysOnly bool) *pb.RequestOp {
		return &pb.RequestOp{Request: &pb.RequestOp_RequestRange{RequestRange: &pb.RangeRequest{Key: []byte(key), RangeEnd: []byte(end), KeysOnly: keysOnly}}}
	}
	deleteOp := func(key string) *pb.RequestOp {
		return &pb.RequestOp{Request: &pb.RequestOp_RequestDeleteRange{RequestDeleteRange: &pb.DeleteRangeRequest{Key: []byte(key), PrevKv: true}}}
	}
	t.Run("constructor rejects unsafe topology", func(t *testing.T) {
		for _, tc := range []struct {
			coord  *grpc.ClientConn
			shards []CoordinatedShard
		}{{nil, shards}, {coordinator, nil}, {coordinator, []CoordinatedShard{{End: []byte("m"), Conn: left}, {Start: []byte("n"), Conn: right}}}, {coordinator, []CoordinatedShard{{Conn: coordinator}}}, {coordinator, []CoordinatedShard{{End: []byte("m"), Conn: left}, {Start: []byte("m"), Conn: left}}}, {coordinator, []CoordinatedShard{{Start: []byte("a"), Conn: left}}}} {
			_, e := NewCoordinatedProxy(ctx, tc.coord, tc.shards)
			require.Error(t, e)
		}
		fallback := append([]CoordinatedShard(nil), shards...)
		for i := range fallback {
			fallback[i].Endpoint = ""
		}
		_, e := NewCoordinatedProxy(ctx, coordinator, fallback)
		require.NoError(t, e)
		expired, done := context.WithCancel(ctx)
		done()
		_, e = NewCoordinatedProxy(expired, coordinator, shards)
		require.Error(t, e)
	})
	t.Run("invalid operations fail before writing blobs", func(t *testing.T) {
		count := func() int64 {
			r, e := pb.NewKVClient(left).Range(ctx, &pb.RangeRequest{Key: []byte(CoordinatedBlobPrefix), RangeEnd: []byte{0}, CountOnly: true})
			require.NoError(t, e)
			return r.Count
		}
		before := count()
		for _, r := range []*pb.PutRequest{nil, {}, {Key: []byte("a"), Value: []byte("x"), IgnoreValue: true}, {Key: []byte("a"), Lease: 3, IgnoreLease: true}} {
			_, e := proxy.Put(ctx, r)
			require.Equal(t, codes.InvalidArgument, status.Code(e))
		}
		for _, r := range []*pb.RangeRequest{nil, {}, {Key: []byte("a"), SortTarget: pb.RangeRequest_VALUE}} {
			_, e := proxy.Range(ctx, r)
			require.Equal(t, codes.InvalidArgument, status.Code(e))
		}
		for _, r := range []*pb.DeleteRangeRequest{nil, {}} {
			_, e := proxy.DeleteRange(ctx, r)
			require.Equal(t, codes.InvalidArgument, status.Code(e))
		}
		for _, r := range []*pb.TxnRequest{nil, {Compare: []*pb.Compare{nil}}, {Compare: []*pb.Compare{{Target: pb.Compare_VALUE, Key: []byte("a")}}}, {Compare: []*pb.Compare{{Key: nil}}}, {Success: []*pb.RequestOp{nil}}, {Success: []*pb.RequestOp{{}}}, {Success: []*pb.RequestOp{{Request: &pb.RequestOp_RequestTxn{RequestTxn: &pb.TxnRequest{}}}}}, {Success: []*pb.RequestOp{{Request: &pb.RequestOp_RequestDeleteRange{}}}}, {Success: []*pb.RequestOp{rangeOp("", "", false)}}, {Success: []*pb.RequestOp{coordinatedPutOp("a", "valid"), rangeOp("", "", false)}}} {
			_, e := proxy.Txn(ctx, r)
			require.Equal(t, codes.InvalidArgument, status.Code(e))
		}
		require.Equal(t, before, count(), "request validation must precede physical staging")
	})
	t.Run("keys only count and readonly transactions preserve metadata", func(t *testing.T) {
		put("a-read", "left")
		put("z-read", "right")
		r, e := kv.Range(ctx, &pb.RangeRequest{Key: []byte("a-read"), RangeEnd: []byte{0}, KeysOnly: true})
		require.NoError(t, e)
		require.Len(t, r.Kvs, 2)
		for _, k := range r.Kvs {
			require.Empty(t, k.Value)
			require.NotZero(t, k.ModRevision)
		}
		counted, e := kv.Range(ctx, &pb.RangeRequest{Key: []byte("a-read"), RangeEnd: []byte{0}, CountOnly: true})
		require.NoError(t, e)
		require.EqualValues(t, 2, counted.Count)
		require.Empty(t, counted.Kvs)
		tr, e := kv.Txn(ctx, &pb.TxnRequest{Success: []*pb.RequestOp{rangeOp("a-read", "", false), rangeOp("z-read", "", true)}})
		require.NoError(t, e)
		require.True(t, tr.Succeeded)
		require.Equal(t, []byte("left"), tr.Responses[0].GetResponseRange().Kvs[0].Value)
		require.Empty(t, tr.Responses[1].GetResponseRange().Kvs[0].Value)
		failed, e := kv.Txn(ctx, &pb.TxnRequest{Compare: []*pb.Compare{{Key: []byte("a-read"), Target: pb.Compare_VERSION, Result: pb.Compare_EQUAL, TargetUnion: &pb.Compare_Version{Version: 0}}}, Failure: []*pb.RequestOp{rangeOp("z-read", "", false)}})
		require.NoError(t, e)
		require.False(t, failed.Succeeded)
		require.Equal(t, []byte("right"), failed.Responses[0].GetResponseRange().Kvs[0].Value)
	})
	t.Run("previous values ignore flags and single shard range deletion", func(t *testing.T) {
		put("b-prev", "first")
		changed, e := kv.Put(ctx, &pb.PutRequest{Key: []byte("b-prev"), IgnoreValue: true, PrevKv: true})
		require.NoError(t, e)
		require.Equal(t, []byte("first"), changed.PrevKv.Value)
		op := coordinatedPutOp("b-prev", "second")
		op.GetRequestPut().PrevKv = true
		tr, e := kv.Txn(ctx, &pb.TxnRequest{Success: []*pb.RequestOp{op}})
		require.NoError(t, e)
		require.Equal(t, []byte("first"), tr.Responses[0].GetResponsePut().PrevKv.Value)
		tr, e = kv.Txn(ctx, &pb.TxnRequest{Success: []*pb.RequestOp{deleteOp("b-prev")}})
		require.NoError(t, e)
		require.Equal(t, []byte("second"), tr.Responses[0].GetResponseDeleteRange().PrevKvs[0].Value)
		put("b-1", "one")
		put("b-2", "two")
		del, e := kv.DeleteRange(ctx, &pb.DeleteRangeRequest{Key: []byte("b-"), RangeEnd: []byte("b."), PrevKv: true})
		require.NoError(t, e)
		require.EqualValues(t, 2, del.Deleted)
		require.Len(t, del.PrevKvs, 2)
		require.Equal(t, []byte("one"), del.PrevKvs[0].Value)
	})
	t.Run("lease APIs expose logical keys and revoke consistently", func(t *testing.T) {
		leases := pb.NewLeaseClient(conn)
		grant, e := leases.LeaseGrant(ctx, &pb.LeaseGrantRequest{TTL: 30})
		require.NoError(t, e)
		_, e = kv.Put(ctx, &pb.PutRequest{Key: []byte("c-lease"), Value: []byte("attached"), Lease: grant.ID})
		require.NoError(t, e)
		_, e = kv.Put(ctx, &pb.PutRequest{Key: []byte("c-lease"), Value: []byte("kept"), IgnoreLease: true})
		require.NoError(t, e)
		ttl, e := leases.LeaseTimeToLive(ctx, &pb.LeaseTimeToLiveRequest{ID: grant.ID, Keys: true})
		require.NoError(t, e)
		require.Equal(t, [][]byte{[]byte("c-lease")}, ttl.Keys)
		all, e := leases.LeaseLeases(ctx, &pb.LeaseLeasesRequest{})
		require.NoError(t, e)
		require.Len(t, all.Leases, 1)
		require.Equal(t, grant.ID, all.Leases[0].ID)
		keep, e := leases.LeaseKeepAlive(ctx)
		require.NoError(t, e)
		require.NoError(t, keep.Send(&pb.LeaseKeepAliveRequest{ID: grant.ID}))
		renewed, e := keep.Recv()
		require.NoError(t, e)
		require.Greater(t, renewed.TTL, int64(0))
		require.NoError(t, keep.CloseSend())
		_, e = leases.LeaseRevoke(ctx, &pb.LeaseRevokeRequest{ID: grant.ID})
		require.NoError(t, e)
		r, e := kv.Range(ctx, &pb.RangeRequest{Key: []byte("c-lease")})
		require.NoError(t, e)
		require.Empty(t, r.Kvs)
		st, e := pb.NewMaintenanceClient(conn).Status(ctx, &pb.StatusRequest{})
		require.NoError(t, e)
		require.NotZero(t, st.Leader)
		poisoned, e := pb.NewLeaseClient(coordinator).LeaseGrant(ctx, &pb.LeaseGrantRequest{TTL: 30})
		require.NoError(t, e)
		_, e = raw.Put(ctx, &pb.PutRequest{Key: []byte("outside-metadata"), Value: []byte("bad"), Lease: poisoned.ID})
		require.NoError(t, e)
		_, e = leases.LeaseTimeToLive(ctx, &pb.LeaseTimeToLiveRequest{ID: poisoned.ID, Keys: true})
		require.Equal(t, codes.DataLoss, status.Code(e))
		expired, done := context.WithCancel(ctx)
		done()
		_, e = proxy.LeaseTimeToLive(expired, &pb.LeaseTimeToLiveRequest{ID: poisoned.ID})
		require.Error(t, e)
	})
	t.Run("corrupt and missing references fail closed", func(t *testing.T) {
		for _, bad := range [][]byte{nil, []byte("invalid-json"), []byte(`{"shard":9,"key":"/__etcd_sharding/blobs/bad"}`), []byte(`{"shard":0,"key":"outside"}`), []byte(`{"shard":0,"key":"/__etcd_sharding/blobs/missing"}`)} {
			corrupt("d-bad", bad)
			_, e := kv.Range(ctx, &pb.RangeRequest{Key: []byte("d-bad")})
			require.Equal(t, codes.DataLoss, status.Code(e))
		}
		_, e := kv.Txn(ctx, &pb.TxnRequest{Success: []*pb.RequestOp{rangeOp("d-bad", "", false)}})
		require.Equal(t, codes.DataLoss, status.Code(e))
		_, e = kv.Put(ctx, &pb.PutRequest{Key: []byte("d-bad"), Value: []byte("new"), PrevKv: true})
		require.Equal(t, codes.DataLoss, status.Code(e))
		corrupt("d-bad", []byte("invalid"))
		_, e = kv.DeleteRange(ctx, &pb.DeleteRangeRequest{Key: []byte("d-bad"), PrevKv: true})
		require.Equal(t, codes.DataLoss, status.Code(e))
		corrupt("d-bad", []byte("invalid"))
		op := coordinatedPutOp("d-bad", "new")
		op.GetRequestPut().PrevKv = true
		_, e = kv.Txn(ctx, &pb.TxnRequest{Success: []*pb.RequestOp{op}})
		require.Equal(t, codes.DataLoss, status.Code(e))
		corrupt("d-bad", []byte("invalid"))
		_, e = kv.Txn(ctx, &pb.TxnRequest{Success: []*pb.RequestOp{deleteOp("d-bad")}})
		require.Equal(t, codes.DataLoss, status.Code(e))
	})
	t.Run("watch cancel malformed create and corrupt event errors", func(t *testing.T) {
		watch, e := pb.NewWatchClient(conn).Watch(ctx)
		require.NoError(t, e)
		require.NoError(t, watch.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: &pb.WatchCreateRequest{Key: []byte("e-watch"), WatchId: 91}}}))
		created, e := watch.Recv()
		require.NoError(t, e)
		require.True(t, created.Created)
		require.NoError(t, watch.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CancelRequest{CancelRequest: &pb.WatchCancelRequest{WatchId: 91}}}))
		canceled, e := watch.Recv()
		require.NoError(t, e)
		require.True(t, canceled.Canceled)
		require.NoError(t, watch.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: &pb.WatchCreateRequest{}}}))
		_, e = watch.Recv()
		require.Equal(t, codes.InvalidArgument, status.Code(e))
		watch, e = pb.NewWatchClient(conn).Watch(ctx)
		require.NoError(t, e)
		require.NoError(t, watch.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: &pb.WatchCreateRequest{Key: []byte("e-watch")}}}))
		_, e = watch.Recv()
		require.NoError(t, e)
		corrupt("e-watch", []byte("broken"))
		_, e = watch.Recv()
		require.Equal(t, codes.DataLoss, status.Code(e))
		watch, e = pb.NewWatchClient(conn).Watch(ctx)
		require.NoError(t, e)
		require.NoError(t, watch.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: &pb.WatchCreateRequest{Key: []byte("e-watch"), PrevKv: true}}}))
		_, e = watch.Recv()
		require.NoError(t, e)
		put("e-watch", "fixed")
		_, e = watch.Recv()
		require.Equal(t, codes.DataLoss, status.Code(e))
	})
	t.Run("canceled backend operations return errors", func(t *testing.T) {
		expired, done := context.WithCancel(ctx)
		done()
		_, e := proxy.DeleteRange(expired, &pb.DeleteRangeRequest{Key: []byte("a")})
		require.Error(t, e)
		_, e = proxy.Txn(expired, &pb.TxnRequest{Success: []*pb.RequestOp{rangeOp("a", "", false)}})
		require.Error(t, e)
		_, e = proxy.Txn(expired, &pb.TxnRequest{Success: []*pb.RequestOp{coordinatedPutOp("a", "x")}})
		require.Error(t, e)
	})
}
