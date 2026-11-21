package goja

import (
	"strings"
	"unsafe"
)

// SetAllocationHooks installs optional callbacks for interpreter allocations.
// check runs before allocating bytes and may panic to stop execution. charge
// receives the start and size of the storage once allocated. The host may use a
// weak reference to the start to account for storage only while it is live.
//
// Hooks cover array storage built by native indexed writes and spread, and
// the string storage allocated or reserved by concatenation and replacement.
// This is not an accounting of the entire runtime heap.
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
	var elem T
	r.checkAllocation(int64(capacity) * int64(unsafe.Sizeof(elem)))
	b := make([]T, length, capacity)
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
	case *concatString:
		// Reserve the maximum size of the flattened string with the rope,
		// which may be materialized later outside the interpreter.
		r.chargeAlloc((*byte)(unsafe.Pointer(s)), int64(s.Length())*2)
	}
}

func (r *Runtime) concat(left, right String) String {
	r.checkAllocation((int64(left.Length()) + int64(right.Length())) * 2)
	result := left.Concat(right)
	r.chargeString(result)
	return result
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
