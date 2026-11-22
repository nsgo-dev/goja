package goja

import (
	"strings"
	"unsafe"
)

// SetAllocationHooks installs optional callbacks for interpreter allocations. check runs before allocating bytes and may panic to stop execution. charge receives the start and size of the storage once allocated. The host may use a weak reference to the start to account for storage only while it is live.
//
// Hooks cover array storage built by native indexed writes and spread, and the string storage allocated by concatenation and replacement. A concatenation that makes a rope is charged for the node and the string it appends, batched along a chain of appends, and the rope is charged its flattened storage once, when it is flattened. Flattening can happen in host code, where execution cannot stop: there a panic from either hook is recovered, the rope is flattened without keeping the result, and the host is expected to have arranged the stop itself, for example with Interrupt, so that the runtime stops at its next check. This is not an accounting of the entire runtime heap.
func (r *Runtime) SetAllocationHooks(check func(int64), charge func(*byte, int64)) {
	r.checkAlloc, r.chargeAlloc = check, charge
}

func (r *Runtime) checkAllocation(bytes int64) {
	if r.checkAlloc != nil {
		r.checkAlloc(bytes)
	}
}

func chargeSlice[T any](r *Runtime, b []T) {
	if r.chargeAlloc != nil && cap(b) != 0 {
		var elem T
		r.chargeAlloc((*byte)(unsafe.Pointer(unsafe.SliceData(b))), int64(cap(b))*int64(unsafe.Sizeof(elem)))
	}
}

func makeMeteredSlice[T any](r *Runtime, length, capacity int) []T {
	return makeMeteredSlice64[T](r, int64(length), int64(capacity))
}

// Check JavaScript lengths before narrowing them on 32-bit platforms.
func makeMeteredSlice64[T any](r *Runtime, length, capacity int64) []T {
	var elem T
	r.checkAllocation(capacity * int64(unsafe.Sizeof(elem)))
	b := make([]T, toIntStrict(length), toIntStrict(capacity))
	chargeSlice(r, b)
	return b
}

func appendMetered[T any](r *Runtime, b []T, value T) []T {
	if r.checkAlloc != nil && len(b) == cap(b) {
		next := makeMeteredSlice[T](r, len(b), growCap(len(b)+1, len(b), cap(b)))
		copy(next, b)
		b = next
	}
	return append(b, value)
}

// ChargeArray accounts for an array's backing store after a host-checked
// operation. A store already charged by an interpreter allocation has the
// same identity, so it must only be charged once.
func (r *Runtime) ChargeArray(value Value) bool {
	if o, ok := value.(*Object); ok {
		switch a := o.self.(type) {
		case *arrayObject:
			chargeSlice(r, a.values)
			return true
		case *sparseArrayObject:
			chargeSlice(r, a.items)
			return true
		}
	}
	return false
}

func (r *Runtime) chargeString(s String) {
	if r.chargeAlloc == nil {
		return
	}
	switch s := s.(type) {
	case asciiString:
		r.chargeAlloc(unsafe.StringData(string(s)), int64(len(s)))
	case unicodeString:
		chargeSlice(r, []uint16(s))
	}
}

const (
	// ropeNodeSize is what a rope node itself takes.
	ropeNodeSize = int64(unsafe.Sizeof(concatString{}))
	// ropeChargeMin batches the charges of a chain of small appends, so that each append does not need an accounting entry of its own.
	ropeChargeMin = 4 << 10
)

// stringStorage is the most bytes s takes when flat.
func stringStorage(s String) int64 {
	if a, ok := s.(asciiString); ok {
		return int64(len(a))
	}
	return int64(s.Length()) * 2
}

// concat charges what a concatenation adds: a rope node and the string it appends, not the left side it already holds. A rope takes over what its left rope had not charged yet, so a chain of small appends is charged once it adds up to ropeChargeMin, and each byte appended is charged once along the chain.
func (r *Runtime) concat(left, right String) String {
	add := ropeNodeSize + stringStorage(right)
	leftRope, _ := left.(*concatString)
	if leftRope != nil {
		// What the chain has not charged yet is charged with this append, so it is checked with it.
		r.checkAllocation(add + leftRope.loadPending())
	} else {
		r.checkAllocation(add)
	}
	result := left.Concat(right)
	rope, ok := result.(*concatString)
	if !ok {
		r.chargeString(result)
		return result
	}
	rope.meter = r
	pending := add
	if leftRope != nil && leftRope != rope {
		pending += leftRope.takePending()
	}
	if pending >= ropeChargeMin && r.chargeAlloc != nil {
		r.chargeAlloc((*byte)(unsafe.Pointer(rope)), pending)
	} else {
		rope.pending = pending
	}
	return result
}

// tryCheckAllocation runs the check hook where execution cannot stop, reporting a panic as a refusal.
func (r *Runtime) tryCheckAllocation(bytes int64) (ok bool) {
	if r.checkAlloc == nil {
		return true
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	r.checkAlloc(bytes)
	return true
}

// tryChargeString charges s where execution cannot stop, recovering a panic from the hook.
func (r *Runtime) tryChargeString(s String) {
	defer func() { _ = recover() }()
	r.chargeString(s)
}

// checkStringGrow covers append's capacity growth and the old buffer still
// live while the replacement buffer is copied. Go's rounding to a size class
// is reflected in the charge after the write.
func (r *Runtime) checkStringGrow(n, capacity, width int) {
	if n > capacity {
		r.checkAllocation(max(int64(n), 2*int64(capacity)) * int64(width))
	}
}

func (r *Runtime) chargeStringBuilder(b *strings.Builder) {
	if r.chargeAlloc != nil && b.Len() != 0 {
		r.chargeAlloc(unsafe.StringData(b.String()), int64(b.Cap()))
	}
}
