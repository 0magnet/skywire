//go:build !tinygo

package registry

import (
	"reflect"
	"testing"
)

// The wire numbering must stay Go's, or every existing schema hash changes.
func TestGoKindsMatchGoNumbering(t *testing.T) {
	for i, k := range goKinds {
		if int(k) != i {
			t.Fatalf("goKinds[%d] = %v (%d)", i, k, k)
		}
		if wireKind(k) != uint32(i) || kindFromWire(uint32(i)) != k { //nolint:gosec
			t.Fatalf("round trip of %v", k)
		}
	}
	if len(goKinds) != int(reflect.UnsafePointer)+1 {
		t.Fatalf("goKinds has %d kinds", len(goKinds))
	}
}
