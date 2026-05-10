package contracttest

import "sync/atomic"

// uniqueCounter is a process-global counter used to give every subtest
// a unique id segment. Combined with a UnixNano timestamp it
// disambiguates subtests that fire in the same nanosecond (rare but
// possible under -parallel).
var uniqueCounter atomicCounter

// atomicCounter is a goroutine-safe monotonic counter starting at 1.
type atomicCounter struct {
	v atomic.Uint64
}

// next returns the next value in the sequence. Goroutine-safe.
func (c *atomicCounter) next() uint64 {
	return c.v.Add(1)
}
