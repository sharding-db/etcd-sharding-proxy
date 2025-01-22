package server

import (
	"github.com/pkg/errors"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

type ResponseFilter interface {
	FilterRange(req *pb.RangeRequest, resps []*pb.RangeResponse) (*pb.RangeResponse, error)
	FilterDeleteRange([]*pb.DeleteRangeResponse) (*pb.DeleteRangeResponse, error)
}

type DefaultResponseFilter struct {
}

func (DefaultResponseFilter) FilterRange(req *pb.RangeRequest, resps []*pb.RangeResponse) (*pb.RangeResponse, error) {
	// assume len(resps) >= 1
	if len(resps) < 2 {
		return resps[0], nil
	}
	ret := resps[0]
	for i := 1; i < len(resps); i++ {
		if req.Limit > 0 && ret.Count >= req.Limit {
			ret.More = true
			break
		}
		if resps[i].Count > 0 {
			if ret.Kvs == nil {
				ret.Kvs = resps[i].Kvs
			}
			ret.Count += resps[i].Count
			ret.Kvs = append(ret.Kvs, resps[i].Kvs...)
		}
		if resps[i].More {
			ret.More = true
		}
	}
	return ret, nil
}

func (DefaultResponseFilter) FilterDeleteRange(resps []*pb.DeleteRangeResponse) (*pb.DeleteRangeResponse, error) {
	if len(resps) == 0 {
		return nil, errors.New("no response")
	}
	ret := resps[0]
	for i := 1; i < len(resps); i++ {
		ret.Deleted += resps[i].Deleted
	}
	return ret, nil
}
