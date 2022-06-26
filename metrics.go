package cache

type Metrics struct {
	Hits       uint64
	Misses     uint64
	Evictions  uint64
	Insertions uint64
}
