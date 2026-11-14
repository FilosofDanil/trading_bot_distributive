package trader

// omap is an insertion-ordered map, the semantics of a Python dict: a key keeps its place when it is set again,
// a deleted key that comes back goes to the end. Python iterates the books, the slots and the shadows in that
// order and the order decides which symbol takes a free slot first, so it has to be kept.
type omap[K comparable, V any] struct {
	m     map[K]V
	order []K
}

func newOmap[K comparable, V any]() *omap[K, V] { return &omap[K, V]{m: map[K]V{}} }

func (o *omap[K, V]) Len() int { return len(o.order) }

func (o *omap[K, V]) Get(k K) (V, bool) {
	v, ok := o.m[k]
	return v, ok
}

func (o *omap[K, V]) Set(k K, v V) {
	if _, ok := o.m[k]; !ok {
		o.order = append(o.order, k)
	}
	o.m[k] = v
}

func (o *omap[K, V]) Delete(k K) {
	if _, ok := o.m[k]; !ok {
		return
	}
	delete(o.m, k)
	for i, x := range o.order {
		if x == k {
			o.order = append(o.order[:i], o.order[i+1:]...)
			break
		}
	}
}

// Keys is a copy of the keys in insertion order.
func (o *omap[K, V]) Keys() []K { return append([]K(nil), o.order...) }

// Values is a copy of the values in insertion order.
func (o *omap[K, V]) Values() []V {
	out := make([]V, 0, len(o.order))
	for _, k := range o.order {
		out = append(out, o.m[k])
	}
	return out
}

func (o *omap[K, V]) Clear() {
	o.m = map[K]V{}
	o.order = nil
}
