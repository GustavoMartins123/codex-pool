package jsonvalue

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAccessors(t *testing.T) {
	object := map[string]any{"k": "v"}
	array := []any{1.0}
	for _, tc := range []struct {
		name  string
		value any
		str   string
		obj   map[string]any
		arr   []any
	}{
		{"string", "text", "text", nil, nil},
		{"object", object, "", object, nil},
		{"array", array, "", nil, array},
		{"number", 1.5, "", nil, nil},
		{"nil", nil, "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := String(tc.value); got != tc.str {
				t.Fatalf("String = %q", got)
			}
			if got := Map(tc.value); !reflect.DeepEqual(got, tc.obj) {
				t.Fatalf("Map = %v", got)
			}
			if got := Slice(tc.value); !reflect.DeepEqual(got, tc.arr) {
				t.Fatalf("Slice = %v", got)
			}
		})
	}
}

func TestInt64(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  int64
	}{
		{float64(42.9), 42}, {int64(7), 7}, {3, 3}, {json.Number("12"), 12},
		{json.Number("1.5"), 0}, {"5", 0}, {nil, 0}, {true, 0},
	} {
		if got := Int64(tc.value); got != tc.want {
			t.Fatalf("Int64(%#v) = %d, want %d", tc.value, got, tc.want)
		}
	}
}
