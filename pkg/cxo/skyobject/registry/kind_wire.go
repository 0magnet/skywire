// Package registry pkg/cxo/skyobject/registry/kind_wire.go c2-net-cxo
package registry

import "reflect"

// goKinds lists reflect kinds in Go's numbering, which is what schemas carry
// on the wire. TinyGo numbers kinds differently, and schema hashes cover them.
var goKinds = [...]reflect.Kind{
	reflect.Invalid, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16,
	reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16,
	reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Float32,
	reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.Array,
	reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer,
	reflect.Slice, reflect.String, reflect.Struct, reflect.UnsafePointer,
}

func wireKind(k reflect.Kind) uint32 {
	for i, gk := range goKinds {
		if gk == k {
			return uint32(i) //nolint:gosec
		}
	}
	return 0
}

func kindFromWire(n uint32) reflect.Kind {
	if int(n) < len(goKinds) {
		return goKinds[n]
	}
	return reflect.Invalid
}
