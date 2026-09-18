package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/gogo/protobuf/proto"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CoordinatedMetadataPrefix reserves coordinator keys for user MVCC metadata.
const CoordinatedMetadataPrefix = "/__etcd_sharding/keys/"

// CoordinatedBlobPrefix reserves data-shard keys for immutable user values.
const CoordinatedBlobPrefix = "/__etcd_sharding/blobs/"
const coordinatedConfigKey = "/__etcd_sharding/config"

// CoordinatedShard describes a fixed half-open key range and its data backend.
// An empty final End covers the remainder of the keyspace.
type CoordinatedShard struct {
	Start, End []byte
	Endpoint   string
	Conn       *grpc.ClientConn
}

// CoordinatedProxy stores MVCC metadata in a coordinator and immutable values in shards.
// The coordinator remains authoritative for ordering, CAS, watch and lease expiry.
type CoordinatedProxy struct {
	pb.UnimplementedMaintenanceServer
	backend *BackendProxy
	shards  []CoordinatedShard
	clients []pb.KVClient
}
type coordinatedRef struct {
	Shard int    `json:"shard"`
	Key   string `json:"key"`
}

// NewCoordinatedProxy requires dedicated clusters and immutable routing. The caller owns connections.
func NewCoordinatedProxy(ctx context.Context, coordinator *grpc.ClientConn, shards []CoordinatedShard) (*CoordinatedProxy, error) {
	if coordinator == nil || len(shards) == 0 {
		return nil, fmt.Errorf("coordinator and shards are required")
	}
	p := &CoordinatedProxy{backend: NewBackendProxy(coordinator)}
	seen := map[string]bool{}
	type route struct {
		Start, End []byte
		Endpoint   string
	}
	routes := []route{}
	for i, s := range shards {
		if s.Conn == nil || (i == 0 && len(s.Start) != 0) || (i > 0 && !bytes.Equal(shards[i-1].End, s.Start)) || (i < len(shards)-1 && (len(s.End) == 0 || bytes.Compare(s.Start, s.End) >= 0)) || (i == len(shards)-1 && len(s.End) != 0) {
			return nil, fmt.Errorf("shards must provide contiguous ordered ranges covering all keys")
		}
		endpoint := s.Endpoint
		if endpoint == "" {
			endpoint = s.Conn.Target()
		}
		if endpoint == coordinator.Target() || s.Conn == coordinator || seen[endpoint] {
			return nil, fmt.Errorf("coordinator and shard endpoints must be distinct")
		}
		seen[endpoint] = true
		s.Start = cloneCoordinatedBytes(s.Start)
		s.End = cloneCoordinatedBytes(s.End)
		p.shards = append(p.shards, s)
		p.clients = append(p.clients, pb.NewKVClient(s.Conn))
		routes = append(routes, route{s.Start, s.End, endpoint})
	}
	encoded, _ := json.Marshal(routes)
	digest := sha256.Sum256(encoded)
	fingerprint := []byte(hex.EncodeToString(digest[:]))
	response, err := p.backend.kv.Txn(backendContext(ctx), &pb.TxnRequest{Compare: []*pb.Compare{{Key: []byte(coordinatedConfigKey), Target: pb.Compare_VERSION, Result: pb.Compare_EQUAL, TargetUnion: &pb.Compare_Version{Version: 0}}}, Success: []*pb.RequestOp{{Request: &pb.RequestOp_RequestPut{RequestPut: &pb.PutRequest{Key: []byte(coordinatedConfigKey), Value: fingerprint}}}}, Failure: []*pb.RequestOp{{Request: &pb.RequestOp_RequestRange{RequestRange: &pb.RangeRequest{Key: []byte(coordinatedConfigKey)}}}}})
	if err != nil {
		return nil, err
	}
	if !response.Succeeded {
		r := response.Responses[0].GetResponseRange()
		if len(r.Kvs) != 1 || !bytes.Equal(r.Kvs[0].Value, fingerprint) {
			return nil, status.Error(codes.FailedPrecondition, "coordinated routing fingerprint mismatch")
		}
	}
	return p, nil
}
func cloneCoordinatedBytes(value []byte) []byte { return append([]byte(nil), value...) }

func coordinatedInvalid(message string) error { return status.Error(codes.InvalidArgument, message) }
func coordinatedKeys(key, end []byte) ([]byte, []byte, error) {
	if len(key) == 0 {
		return nil, nil, coordinatedInvalid("empty key")
	}
	k := append([]byte(CoordinatedMetadataPrefix), key...)
	var e []byte
	if len(end) > 0 {
		if bytes.Equal(end, []byte{0}) {
			e = []byte("/__etcd_sharding/keys0")
		} else {
			e = append([]byte(CoordinatedMetadataPrefix), end...)
		}
	}
	return k, e, nil
}
func (p *CoordinatedProxy) shard(key []byte) int {
	for i, s := range p.shards {
		if len(s.End) == 0 || bytes.Compare(key, s.End) < 0 {
			return i
		}
	}
	panic("validated routing")
}
func (p *CoordinatedProxy) mutationShard(key, end []byte) (int, error) {
	if _, _, e := coordinatedKeys(key, end); e != nil {
		return 0, e
	}
	i := p.shard(key)
	if len(end) > 0 && len(p.shards[i].End) > 0 && (bytes.Equal(end, []byte{0}) || bytes.Compare(end, p.shards[i].End) > 0) {
		return 0, coordinatedInvalid("cross-shard mutation unsupported")
	}
	return i, nil
}
func (p *CoordinatedProxy) decodeKV(ctx context.Context, kv *mvccpb.KeyValue, keysOnly bool) error {
	if kv == nil {
		return nil
	}
	if !bytes.HasPrefix(kv.Key, []byte(CoordinatedMetadataPrefix)) {
		return status.Error(codes.DataLoss, "unexpected coordinator key")
	}
	kv.Key = cloneCoordinatedBytes(kv.Key[len(CoordinatedMetadataPrefix):])
	if keysOnly {
		return nil
	}
	if len(kv.Value) == 0 {
		return status.Error(codes.DataLoss, "empty immutable blob reference")
	}
	var ref coordinatedRef
	if e := json.Unmarshal(kv.Value, &ref); e != nil || ref.Shard < 0 || ref.Shard >= len(p.clients) || !bytes.HasPrefix([]byte(ref.Key), []byte(CoordinatedBlobPrefix)) {
		return status.Error(codes.DataLoss, "invalid immutable blob reference")
	}
	r, e := p.clients[ref.Shard].Range(backendContext(ctx), &pb.RangeRequest{Key: []byte(ref.Key)})
	if e != nil {
		return e
	}
	if len(r.Kvs) != 1 {
		return status.Error(codes.DataLoss, "immutable blob missing")
	}
	kv.Value = cloneCoordinatedBytes(r.Kvs[0].Value)
	return nil
}
func (p *CoordinatedProxy) rangeRequest(req *pb.RangeRequest) (*pb.RangeRequest, error) {
	if req == nil {
		return nil, coordinatedInvalid("missing range")
	}
	if req.SortTarget == pb.RangeRequest_VALUE {
		return nil, coordinatedInvalid("value sorting unsupported")
	}
	r := proto.Clone(req).(*pb.RangeRequest)
	var e error
	r.Key, r.RangeEnd, e = coordinatedKeys(r.Key, r.RangeEnd)
	return r, e
}
func (p *CoordinatedProxy) Range(ctx context.Context, req *pb.RangeRequest) (*pb.RangeResponse, error) {
	r, e := p.rangeRequest(req)
	if e != nil {
		return nil, e
	}
	out, e := forwardUnary(ctx, r, p.backend.kv.Range)
	if e != nil {
		return nil, e
	}
	for _, kv := range out.Kvs {
		if e = p.decodeKV(ctx, kv, req.KeysOnly); e != nil {
			return nil, e
		}
	}
	return out, nil
}
func validateCoordinatedPut(req *pb.PutRequest) error {
	if req == nil {
		return coordinatedInvalid("missing put")
	}
	if _, _, e := coordinatedKeys(req.Key, nil); e != nil {
		return e
	}
	if req.IgnoreValue && len(req.Value) > 0 {
		return coordinatedInvalid("value provided with ignore_value")
	}
	if req.IgnoreLease && req.Lease != 0 {
		return coordinatedInvalid("lease provided with ignore_lease")
	}
	return nil
}

// stagePut persists the value before publishing its reference. A failed or losing
// commit leaves an unreachable blob; blobs are never overwritten or garbage collected.
func (p *CoordinatedProxy) stagePut(ctx context.Context, req *pb.PutRequest) (*pb.PutRequest, error) {
	r := proto.Clone(req).(*pb.PutRequest)
	k, _, e := coordinatedKeys(req.Key, nil)
	if e != nil {
		return nil, e
	}
	r.Key = k
	if !r.IgnoreValue {
		token := make([]byte, 32)
		if _, e = rand.Read(token); e != nil {
			return nil, e
		}
		i := p.shard(req.Key)
		blob := CoordinatedBlobPrefix + hex.EncodeToString(token)
		if _, e = p.clients[i].Put(backendContext(ctx), &pb.PutRequest{Key: []byte(blob), Value: req.Value}); e != nil {
			return nil, e
		}
		r.Value, _ = json.Marshal(coordinatedRef{i, blob})
	}
	return r, nil
}
func (p *CoordinatedProxy) Put(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error) {
	if e := validateCoordinatedPut(req); e != nil {
		return nil, e
	}
	r, e := p.stagePut(ctx, req)
	if e != nil {
		return nil, e
	}
	out, e := forwardUnary(ctx, r, p.backend.kv.Put)
	if e != nil {
		return nil, e
	}
	if e = p.decodeKV(ctx, out.PrevKv, false); e != nil {
		return nil, e
	}
	return out, nil
}
func coordinatedDelete(req *pb.DeleteRangeRequest) (*pb.DeleteRangeRequest, error) {
	if req == nil {
		return nil, coordinatedInvalid("missing delete")
	}
	r := proto.Clone(req).(*pb.DeleteRangeRequest)
	var e error
	r.Key, r.RangeEnd, e = coordinatedKeys(r.Key, r.RangeEnd)
	return r, e
}
func (p *CoordinatedProxy) DeleteRange(ctx context.Context, req *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, error) {
	if req == nil {
		return nil, coordinatedInvalid("missing delete")
	}
	if _, e := p.mutationShard(req.Key, req.RangeEnd); e != nil {
		return nil, e
	}
	r, e := coordinatedDelete(req)
	if e != nil {
		return nil, e
	}
	out, e := forwardUnary(ctx, r, p.backend.kv.DeleteRange)
	if e != nil {
		return nil, e
	}
	for _, kv := range out.PrevKvs {
		if e = p.decodeKV(ctx, kv, false); e != nil {
			return nil, e
		}
	}
	return out, nil
}
func (p *CoordinatedProxy) Txn(ctx context.Context, req *pb.TxnRequest) (*pb.TxnResponse, error) {
	if req == nil {
		return nil, coordinatedInvalid("missing txn")
	}
	r := proto.Clone(req).(*pb.TxnRequest)
	for _, c := range r.Compare {
		if c == nil {
			return nil, coordinatedInvalid("missing compare")
		}
		if c.Target == pb.Compare_VALUE {
			return nil, coordinatedInvalid("value comparisons unsupported")
		}
		var e error
		c.Key, c.RangeEnd, e = coordinatedKeys(c.Key, c.RangeEnd)
		if e != nil {
			return nil, e
		}
	}
	// Validate both branches before staging any values. Inactive branches may also
	// stage unreachable blobs, but can never expose partially committed metadata.
	mutation := -1
	for _, branch := range [][]*pb.RequestOp{r.Success, r.Failure} {
		for _, op := range branch {
			if op == nil {
				return nil, coordinatedInvalid("missing operation")
			}
			index := -1
			var e error
			switch v := op.Request.(type) {
			case *pb.RequestOp_RequestRange:
				_, e = p.rangeRequest(v.RequestRange)
			case *pb.RequestOp_RequestPut:
				e = validateCoordinatedPut(v.RequestPut)
				if e == nil {
					index, e = p.mutationShard(v.RequestPut.Key, nil)
				}
			case *pb.RequestOp_RequestDeleteRange:
				if v.RequestDeleteRange == nil {
					return nil, coordinatedInvalid("missing delete")
				}
				index, e = p.mutationShard(v.RequestDeleteRange.Key, v.RequestDeleteRange.RangeEnd)
			default:
				return nil, coordinatedInvalid("nested or unknown transaction operation unsupported")
			}
			if e != nil {
				return nil, e
			}
			if index >= 0 {
				if mutation >= 0 && mutation != index {
					return nil, coordinatedInvalid("cross-shard mutating transaction unsupported")
				}
				mutation = index
			}
		}
	}
	for _, branch := range [][]*pb.RequestOp{r.Success, r.Failure} {
		for _, op := range branch {
			var e error
			switch v := op.Request.(type) {
			case *pb.RequestOp_RequestRange:
				v.RequestRange, e = p.rangeRequest(v.RequestRange)
			case *pb.RequestOp_RequestPut:
				v.RequestPut, e = p.stagePut(ctx, v.RequestPut)
			case *pb.RequestOp_RequestDeleteRange:
				v.RequestDeleteRange, e = coordinatedDelete(v.RequestDeleteRange)
			}
			if e != nil {
				return nil, e
			}
		}
	}
	out, e := forwardUnary(ctx, r, p.backend.kv.Txn)
	if e != nil {
		return nil, e
	}
	branch := req.Failure
	if out.Succeeded {
		branch = req.Success
	}
	for i, op := range out.Responses {
		switch v := op.Response.(type) {
		case *pb.ResponseOp_ResponseRange:
			for _, kv := range v.ResponseRange.Kvs {
				if e = p.decodeKV(ctx, kv, branch[i].GetRequestRange().KeysOnly); e != nil {
					return nil, e
				}
			}
		case *pb.ResponseOp_ResponsePut:
			if e = p.decodeKV(ctx, v.ResponsePut.PrevKv, false); e != nil {
				return nil, e
			}
		case *pb.ResponseOp_ResponseDeleteRange:
			for _, kv := range v.ResponseDeleteRange.PrevKvs {
				if e = p.decodeKV(ctx, kv, false); e != nil {
					return nil, e
				}
			}
		}
	}
	return out, nil
}
func (p *CoordinatedProxy) Compact(ctx context.Context, r *pb.CompactionRequest) (*pb.CompactionResponse, error) {
	return p.backend.Compact(ctx, r)
}
func (p *CoordinatedProxy) LeaseGrant(ctx context.Context, r *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	return p.backend.LeaseGrant(ctx, r)
}
func (p *CoordinatedProxy) LeaseRevoke(ctx context.Context, r *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	return p.backend.LeaseRevoke(ctx, r)
}
func (p *CoordinatedProxy) LeaseLeases(ctx context.Context, r *pb.LeaseLeasesRequest) (*pb.LeaseLeasesResponse, error) {
	return p.backend.LeaseLeases(ctx, r)
}
func (p *CoordinatedProxy) LeaseTimeToLive(ctx context.Context, r *pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error) {
	out, e := p.backend.LeaseTimeToLive(ctx, r)
	if e != nil {
		return nil, e
	}
	for i, k := range out.Keys {
		if !bytes.HasPrefix(k, []byte(CoordinatedMetadataPrefix)) {
			return nil, status.Error(codes.DataLoss, "unexpected lease key")
		}
		out.Keys[i] = cloneCoordinatedBytes(k[len(CoordinatedMetadataPrefix):])
	}
	return out, nil
}
func (p *CoordinatedProxy) LeaseKeepAlive(s pb.Lease_LeaseKeepAliveServer) error {
	return p.backend.LeaseKeepAlive(s)
}
func (p *CoordinatedProxy) Status(ctx context.Context, r *pb.StatusRequest) (*pb.StatusResponse, error) {
	return p.backend.Status(ctx, r)
}

type coordinatedWatchStream struct {
	grpc.ClientStream
	p   *CoordinatedProxy
	ctx context.Context
}

func (s *coordinatedWatchStream) SendMsg(message interface{}) error {
	r := proto.Clone(message.(*pb.WatchRequest)).(*pb.WatchRequest)
	if c := r.GetCreateRequest(); c != nil {
		var e error
		c.Key, c.RangeEnd, e = coordinatedKeys(c.Key, c.RangeEnd)
		if e != nil {
			return e
		}
	}
	return s.ClientStream.SendMsg(r)
}
func (s *coordinatedWatchStream) RecvMsg(message interface{}) error {
	if e := s.ClientStream.RecvMsg(message); e != nil {
		return e
	}
	for _, event := range message.(*pb.WatchResponse).Events {
		if e := s.p.decodeKV(s.ctx, event.Kv, event.Type == mvccpb.DELETE); e != nil {
			return e
		}
		if e := s.p.decodeKV(s.ctx, event.PrevKv, false); e != nil {
			return e
		}
	}
	return nil
}
func (p *CoordinatedProxy) Watch(front pb.Watch_WatchServer) error {
	ctx, cancel := context.WithCancel(backendContext(front.Context()))
	defer cancel()
	back, e := p.backend.watch.Watch(ctx)
	if e != nil {
		return e
	}
	return forwardBidi(front, &coordinatedWatchStream{back, p, ctx}, cancel, func() interface{} { return new(pb.WatchRequest) }, func() interface{} { return new(pb.WatchResponse) })
}

var (
	_ pb.KVServer          = (*CoordinatedProxy)(nil)
	_ pb.WatchServer       = (*CoordinatedProxy)(nil)
	_ pb.LeaseServer       = (*CoordinatedProxy)(nil)
	_ pb.MaintenanceServer = (*CoordinatedProxy)(nil)
)
