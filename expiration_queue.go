package cache

import (
	"container/heap"
	"container/list"
)

type expirationQueue[K comparable, V any] []*list.Element

func newExpirationQueue[K comparable, V any]() expirationQueue[K, V] {
	q := make(expirationQueue[K, V], 0)
	heap.Init(&q)
	return q
}

func (q expirationQueue[K, V]) isEmpty() bool {
	return q.Len() == 0
}

func (q *expirationQueue[K, V]) update(elem *list.Element) {
	heap.Fix(q, elem.Value.(*Item[K, V]).queueIndex)
}

func (q *expirationQueue[K, V]) push(elem *list.Element) {
	heap.Push(q, elem)
}

func (q *expirationQueue[K, V]) remove(elem *list.Element) {
	heap.Remove(q, elem.Value.(*Item[K, V]).queueIndex)
}

func (q expirationQueue[K, V]) Len() int {
	return len(q)
}

func (q expirationQueue[K, V]) Less(i, j int) bool {

	item1, item2 := q[i].Value.(*Item[K, V]), q[j].Value.(*Item[K, V])
	if item1.expiresAt.IsZero() {
		return false
	}
	if item2.expiresAt.IsZero() {
		return true
	}
	return item1.expiresAt.Before(item2.expiresAt)
}

func (q expirationQueue[K, V]) Swap(i, j int) {

	q[i], q[j] = q[j], q[i]
	q[i].Value.(*Item[K, V]).queueIndex = i
	q[j].Value.(*Item[K, V]).queueIndex = j
}

func (q *expirationQueue[K, V]) Push(x interface{}) {

	elem := x.(*list.Element)
	elem.Value.(*Item[K, V]).queueIndex = len(*q)
	*q = append(*q, elem)
}

func (q *expirationQueue[K, V]) Pop() interface{} {

	old := *q
	i := len(old) - 1
	elem := old[i]
	elem.Value.(*Item[K, V]).queueIndex = -1
	old[i] = nil
	*q = old[:i]
	return elem
}
