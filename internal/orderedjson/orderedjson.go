// Package orderedjson marshals JSON objects with a caller-specified key order.
package orderedjson

import (
	"encoding/json"
	"sort"
)

// Marshal produces JSON from a map with keys emitted in the specified order.
// Keys in order but absent from m are skipped. Keys in m but not in order are
// appended alphabetically after the ordered keys.
func Marshal(m map[string]any, order []string) ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}

	seen := make(map[string]bool, len(order))
	var buf []byte
	buf = append(buf, '{')

	first := true
	// Emit keys in specified order
	for _, key := range order {
		val, ok := m[key]
		if !ok {
			continue
		}
		seen[key] = true
		if !first {
			buf = append(buf, ',')
		}
		first = false
		keyBytes, _ := json.Marshal(key)
		valBytes, err := json.Marshal(val)
		if err != nil {
			return nil, err
		}
		buf = append(buf, keyBytes...)
		buf = append(buf, ':')
		buf = append(buf, valBytes...)
	}

	// Emit remaining keys alphabetically
	remaining := make([]string, 0, len(m)-len(seen))
	for key := range m {
		if !seen[key] {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)

	for _, key := range remaining {
		if !first {
			buf = append(buf, ',')
		}
		first = false
		keyBytes, _ := json.Marshal(key)
		valBytes, err := json.Marshal(m[key])
		if err != nil {
			return nil, err
		}
		buf = append(buf, keyBytes...)
		buf = append(buf, ':')
		buf = append(buf, valBytes...)
	}

	buf = append(buf, '}')
	return buf, nil
}
