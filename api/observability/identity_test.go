package observability

import (
	"reflect"
	"testing"
)

func TestOperationHandleExportsOnlyPublicID(t *testing.T) {
	h := NewOperationHandle("op-1")
	if h.ID() != "op-1" {
		t.Fatalf("ID() = %q, want op-1", h.ID())
	}
	if h.String() != "op-1" {
		t.Fatalf("String() = %q, want op-1", h.String())
	}
	if h.IsZero() {
		t.Fatal("handle should not be zero")
	}

	typ := reflect.TypeOf(h)
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Type == reflect.TypeOf(InternalOperationIdentity(0)) {
			t.Fatalf("OperationHandle field %s exposes InternalOperationIdentity", field.Name)
		}
	}
}

func TestOperationIdentityInputOmitsNodeID(t *testing.T) {
	typ := reflect.TypeOf(OperationIdentityInput{})
	if _, ok := typ.FieldByName("NodeID"); ok {
		t.Fatal("OperationIdentityInput must not expose NodeID in the first implementation")
	}
}

func TestOperationIdentityRef(t *testing.T) {
	identity := OperationIdentity{ID: "child", ParentID: "parent"}
	ref := identity.Ref()
	if ref.ID != "child" || ref.ParentID != "parent" {
		t.Fatalf("Ref() = %+v, want child/parent", ref)
	}
}
