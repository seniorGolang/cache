package cache

import "time"

type Option[K comparable, V any] interface {
	apply(opts *options[K, V])
}

type optionFunc[K comparable, V any] func(*options[K, V])

func (fn optionFunc[K, V]) apply(opts *options[K, V]) {
	fn(opts)
}

type options[K comparable, V any] struct {
	capacity          uint64
	ttl               time.Duration
	loader            Loader[K, V]
	disableTouchOnHit bool
}

func applyOptions[K comparable, V any](v *options[K, V], opts ...Option[K, V]) {

	for i := range opts {
		opts[i].apply(v)
	}
}

func WithCapacity[K comparable, V any](c uint64) Option[K, V] {

	return optionFunc[K, V](func(opts *options[K, V]) {
		opts.capacity = c
	})
}

func WithTTL[K comparable, V any](ttl time.Duration) Option[K, V] {

	return optionFunc[K, V](func(opts *options[K, V]) {
		opts.ttl = ttl
	})
}

func WithLoader[K comparable, V any](l Loader[K, V]) Option[K, V] {

	return optionFunc[K, V](func(opts *options[K, V]) {
		opts.loader = l
	})
}

func WithDisableTouchOnHit[K comparable, V any]() Option[K, V] {

	return optionFunc[K, V](func(opts *options[K, V]) {
		opts.disableTouchOnHit = true
	})
}
