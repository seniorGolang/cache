package cache

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	EvictionReasonDeleted EvictionReason = iota + 1
	EvictionReasonExpired
	EvictionReasonCapacityReached
)

type EvictionReason int

type Cache[K comparable, V any] struct {
	items struct {
		lru      *list.List
		mu       sync.RWMutex
		values   map[K]*list.Element
		timerCh  chan time.Duration
		expQueue expirationQueue[K, V]
	}
	metrics   Metrics
	metricsMu sync.RWMutex
	events    struct {
		insertion struct {
			mu     sync.RWMutex
			nextID uint64
			fns    map[uint64]func(*Item[K, V])
		}
		eviction struct {
			mu     sync.RWMutex
			nextID uint64
			fns    map[uint64]func(EvictionReason, *Item[K, V])
		}
	}
	stopCh  chan struct{}
	options options[K, V]
}

func New[K comparable, V any](opts ...Option[K, V]) *Cache[K, V] {

	c := &Cache[K, V]{
		stopCh: make(chan struct{}),
	}
	c.items.values = make(map[K]*list.Element)
	c.items.lru = list.New()
	c.items.expQueue = newExpirationQueue[K, V]()
	c.items.timerCh = make(chan time.Duration, 1)
	c.events.insertion.fns = make(map[uint64]func(*Item[K, V]))
	c.events.eviction.fns = make(map[uint64]func(EvictionReason, *Item[K, V]))
	applyOptions(&c.options, opts...)
	return c
}

func (c *Cache[K, V]) updateExpirations(fresh bool, elem *list.Element) {

	var oldExpiresAt time.Time
	if !c.items.expQueue.isEmpty() {
		oldExpiresAt = c.items.expQueue[0].Value.(*Item[K, V]).expiresAt
	}
	if fresh {
		c.items.expQueue.push(elem)
	} else {
		c.items.expQueue.update(elem)
	}
	newExpiresAt := c.items.expQueue[0].Value.(*Item[K, V]).expiresAt
	if newExpiresAt.IsZero() || (!oldExpiresAt.IsZero() && !newExpiresAt.Before(oldExpiresAt)) {
		return
	}
	d := time.Until(newExpiresAt)
	if len(c.items.timerCh) > 0 {
		select {
		case d1 := <-c.items.timerCh:
			if d1 < d {
				d = d1
			}
		default:
		}
	}
	c.items.timerCh <- d
}

func (c *Cache[K, V]) set(key K, value V, ttl time.Duration) *Item[K, V] {

	if ttl == DefaultTTL {
		ttl = c.options.ttl
	}
	elem := c.get(key, false)
	if elem != nil {
		item := elem.Value.(*Item[K, V])
		item.update(value, ttl)
		c.updateExpirations(false, elem)
		return item
	}
	if c.options.capacity != 0 && uint64(len(c.items.values)) >= c.options.capacity {
		c.evict(EvictionReasonCapacityReached, c.items.lru.Back())
	}
	item := newItem(key, value, ttl)
	elem = c.items.lru.PushFront(item)
	c.items.values[key] = elem
	c.updateExpirations(true, elem)
	c.metricsMu.Lock()
	c.metrics.Insertions++
	c.metricsMu.Unlock()
	c.events.insertion.mu.RLock()
	for _, fn := range c.events.insertion.fns {
		fn(item)
	}
	c.events.insertion.mu.RUnlock()
	return item
}

func (c *Cache[K, V]) get(key K, touch bool) *list.Element {

	elem := c.items.values[key]
	if elem == nil {
		return nil
	}
	item := elem.Value.(*Item[K, V])
	if item.isExpiredUnsafe() {
		return nil
	}
	c.items.lru.MoveToFront(elem)
	if touch && item.ttl > 0 {
		item.touch()
		c.updateExpirations(false, elem)
	}
	return elem
}

func (c *Cache[K, V]) evict(reason EvictionReason, elems ...*list.Element) {

	if len(elems) > 0 {
		c.metricsMu.Lock()
		c.metrics.Evictions += uint64(len(elems))
		c.metricsMu.Unlock()
		c.events.eviction.mu.RLock()
		for i := range elems {
			item := elems[i].Value.(*Item[K, V])
			delete(c.items.values, item.key)
			c.items.lru.Remove(elems[i])
			c.items.expQueue.remove(elems[i])

			for _, fn := range c.events.eviction.fns {
				fn(reason, item)
			}
		}
		c.events.eviction.mu.RUnlock()
		return
	}
	c.metricsMu.Lock()
	c.metrics.Evictions += uint64(len(c.items.values))
	c.metricsMu.Unlock()
	c.events.eviction.mu.RLock()
	for _, elem := range c.items.values {
		item := elem.Value.(*Item[K, V])
		for _, fn := range c.events.eviction.fns {
			fn(reason, item)
		}
	}
	c.events.eviction.mu.RUnlock()
	c.items.values = make(map[K]*list.Element)
	c.items.lru.Init()
	c.items.expQueue = newExpirationQueue[K, V]()
}

func (c *Cache[K, V]) Set(key K, value V, ttl time.Duration) *Item[K, V] {

	c.items.mu.Lock()
	defer c.items.mu.Unlock()
	return c.set(key, value, ttl)
}

func (c *Cache[K, V]) Get(key K, opts ...Option[K, V]) *Item[K, V] {

	getOpts := options[K, V]{
		loader:            c.options.loader,
		disableTouchOnHit: c.options.disableTouchOnHit,
	}
	applyOptions(&getOpts, opts...)
	c.items.mu.Lock()
	elem := c.get(key, !getOpts.disableTouchOnHit)
	c.items.mu.Unlock()
	if elem == nil {
		c.metricsMu.Lock()
		c.metrics.Misses++
		c.metricsMu.Unlock()
		if getOpts.loader != nil {
			return getOpts.loader.Load(c, key)
		}
		return nil
	}
	c.metricsMu.Lock()
	c.metrics.Hits++
	c.metricsMu.Unlock()
	return elem.Value.(*Item[K, V])
}

func (c *Cache[K, V]) Delete(key K) {

	c.items.mu.Lock()
	defer c.items.mu.Unlock()
	elem := c.items.values[key]
	if elem == nil {
		return
	}
	c.evict(EvictionReasonDeleted, elem)
}

func (c *Cache[K, V]) DeleteAll() {

	c.items.mu.Lock()
	c.evict(EvictionReasonDeleted)
	c.items.mu.Unlock()
}

func (c *Cache[K, V]) DeleteExpired() {

	c.items.mu.Lock()
	defer c.items.mu.Unlock()
	if c.items.expQueue.isEmpty() {
		return
	}
	e := c.items.expQueue[0]
	for e.Value.(*Item[K, V]).isExpiredUnsafe() {
		c.evict(EvictionReasonExpired, e)
		if c.items.expQueue.isEmpty() {
			break
		}
		e = c.items.expQueue[0]
	}
}

func (c *Cache[K, V]) Touch(key K) {

	c.items.mu.Lock()
	c.get(key, true)
	c.items.mu.Unlock()
}

func (c *Cache[K, V]) Len() int {

	c.items.mu.RLock()
	defer c.items.mu.RUnlock()
	return len(c.items.values)
}

func (c *Cache[K, V]) Keys() []K {

	c.items.mu.RLock()
	defer c.items.mu.RUnlock()
	res := make([]K, 0, len(c.items.values))
	for k := range c.items.values {
		res = append(res, k)
	}
	return res
}

func (c *Cache[K, V]) Items() map[K]*Item[K, V] {

	c.items.mu.RLock()
	defer c.items.mu.RUnlock()
	items := make(map[K]*Item[K, V], len(c.items.values))
	for k := range c.items.values {
		item := c.get(k, false)
		if item != nil {
			items[k] = item.Value.(*Item[K, V])
		}
	}
	return items
}

func (c *Cache[K, V]) Metrics() Metrics {

	c.metricsMu.RLock()
	defer c.metricsMu.RUnlock()
	return c.metrics
}

func (c *Cache[K, V]) Start() {

	waitDur := func() time.Duration {
		c.items.mu.RLock()
		defer c.items.mu.RUnlock()
		if !c.items.expQueue.isEmpty() &&
			!c.items.expQueue[0].Value.(*Item[K, V]).expiresAt.IsZero() {
			d := time.Until(c.items.expQueue[0].Value.(*Item[K, V]).expiresAt)
			if d <= 0 {
				// execute immediately
				return time.Microsecond
			}
			return d
		}
		if c.options.ttl > 0 {
			return c.options.ttl
		}
		return time.Hour
	}
	timer := time.NewTimer(waitDur())
	stop := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	defer stop()
	for {
		select {
		case <-c.stopCh:
			return
		case d := <-c.items.timerCh:
			stop()
			timer.Reset(d)
		case <-timer.C:
			c.DeleteExpired()
			stop()
			timer.Reset(waitDur())
		}
	}
}

func (c *Cache[K, V]) Stop() {
	c.stopCh <- struct{}{}
}

func (c *Cache[K, V]) OnInsertion(fn func(context.Context, *Item[K, V])) func() {

	var (
		wg          sync.WaitGroup
		ctx, cancel = context.WithCancel(context.Background())
	)
	c.events.insertion.mu.Lock()
	id := c.events.insertion.nextID
	c.events.insertion.fns[id] = func(item *Item[K, V]) {
		wg.Add(1)
		go func() {
			fn(ctx, item)
			wg.Done()
		}()
	}
	c.events.insertion.nextID++
	c.events.insertion.mu.Unlock()
	return func() {
		cancel()
		c.events.insertion.mu.Lock()
		delete(c.events.insertion.fns, id)
		c.events.insertion.mu.Unlock()
		wg.Wait()
	}
}

func (c *Cache[K, V]) OnEviction(fn func(context.Context, EvictionReason, *Item[K, V])) func() {

	var (
		wg          sync.WaitGroup
		ctx, cancel = context.WithCancel(context.Background())
	)
	c.events.eviction.mu.Lock()
	id := c.events.eviction.nextID
	c.events.eviction.fns[id] = func(r EvictionReason, item *Item[K, V]) {
		wg.Add(1)
		go func() {
			fn(ctx, r, item)
			wg.Done()
		}()
	}
	c.events.eviction.nextID++
	c.events.eviction.mu.Unlock()

	return func() {
		cancel()
		c.events.eviction.mu.Lock()
		delete(c.events.eviction.fns, id)
		c.events.eviction.mu.Unlock()
		wg.Wait()
	}
}

type Loader[K comparable, V any] interface {
	Load(c *Cache[K, V], key K) *Item[K, V]
}

type LoaderFunc[K comparable, V any] func(*Cache[K, V], K) *Item[K, V]

func (l LoaderFunc[K, V]) Load(c *Cache[K, V], key K) *Item[K, V] {
	return l(c, key)
}

type SuppressedLoader[K comparable, V any] struct {
	Loader[K, V]
	group *singleflight.Group
}

func (l *SuppressedLoader[K, V]) Load(c *Cache[K, V], key K) *Item[K, V] {

	strKey := fmt.Sprint(key)
	res, _, _ := l.group.Do(strKey, func() (interface{}, error) {
		item := l.Loader.Load(c, key)
		if item == nil {
			return nil, nil
		}
		return item, nil
	})
	if res == nil {
		return nil
	}
	return res.(*Item[K, V])
}
