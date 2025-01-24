package server

import (
	"math"
	"sync/atomic"
	"time"
)

// file copied from etcd v3.5

const (
	tsLen     = 5 * 8
	cntLen    = 8
	suffixLen = tsLen + cntLen
)

var singletonGenerator *Generator = NewGenerator(0, time.Now())

func GetIDGenerator() *Generator {
	return singletonGenerator
}

type Generator struct {
	// high order 2 bytes
	prefix uint64
	// low order 6 bytes
	suffix uint64
}

func NewGenerator(memberID uint16, now time.Time) *Generator {
	prefix := uint64(memberID) << suffixLen
	unixMilli := uint64(now.UnixNano()) / uint64(time.Millisecond/time.Nanosecond)
	suffix := lowbit(unixMilli, tsLen) << cntLen
	return &Generator{
		prefix: prefix,
		suffix: suffix,
	}
}

// Next generates a id that is unique.
func (g *Generator) Next() uint64 {
	suffix := atomic.AddUint64(&g.suffix, 1)
	id := g.prefix | lowbit(suffix, suffixLen)
	return id
}

func (g *Generator) NextInt64() int64 {
	// use the low 63 bits to avoid negative number
	return int64(g.Next() & ((1 << 63) - 1))
}

func lowbit(x uint64, n uint) uint64 {
	return x & (math.MaxUint64 >> (64 - n))
}
