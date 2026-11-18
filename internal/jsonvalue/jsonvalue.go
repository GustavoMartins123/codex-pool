// Package jsonvalue reads values decoded from JSON into interface values.
package jsonvalue

import "encoding/json"

// String returns v when it is a string, or "".
func String(v any) string {
	s, _ := v.(string)
	return s
}

// Map returns v when it is a JSON object, or nil.
func Map(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// Slice returns v when it is a JSON array, or nil.
func Slice(v any) []any {
	s, _ := v.([]any)
	return s
}

// Int64 converts a JSON number to an int64, or returns 0.
func Int64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}
