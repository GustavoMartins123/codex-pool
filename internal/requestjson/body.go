// Package requestjson inspects and rewrites JSON request bodies.
package requestjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func ConversationID(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(blob, &obj); err != nil {
		return ""
	}
	return ConversationIDFromObject(obj)
}

func ConversationIDFromObject(obj map[string]any) string {
	for _, key := range []string{"conversation_id", "conversation", "session_id", "prompt_cache_key"} {
		if v, ok := obj[key].(string); ok && v != "" {
			return v
		}
	}
	for _, containerKey := range []string{"client_metadata", "metadata", "meta"} {
		if sub, ok := obj[containerKey].(map[string]any); ok {
			for _, key := range []string{"conversation_id", "conversation", "session_id", "user_id"} {
				if v, ok := sub[key].(string); ok && v != "" {
					return v
				}
			}
		}
	}
	return ""
}

func RequestedModel(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(blob, &obj); err != nil {
		return ""
	}
	if v, ok := obj["model"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func ReadBodyPrefix(body io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	_, err := io.CopyN(&buf, body, limit)
	if err == io.EOF {
		err = nil
	}
	return buf.Bytes(), err
}

type PrefixReadCloser struct {
	R io.Reader
	C io.Closer
}

func (p *PrefixReadCloser) Read(b []byte) (int, error) {
	return p.R.Read(b)
}

func (p *PrefixReadCloser) Close() error {
	return p.C.Close()
}

func ReplaceStringToken(prefix []byte, start, end int, value string) ([]byte, int, error) {
	if start < 0 || end < start || end > len(prefix) {
		return nil, 0, fmt.Errorf("invalid JSON string bounds")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, 0, err
	}
	out := make([]byte, 0, len(prefix)-(end-start)+len(encoded))
	out = append(out, prefix[:start]...)
	out = append(out, encoded...)
	out = append(out, prefix[end:]...)
	return out, len(encoded) - (end - start), nil
}

func FindTopLevelStringField(blob []byte, field string) (value string, valueStart int, valueEnd int, ok bool) {
	i := skipJSONWhitespace(blob, 0)
	if i >= len(blob) || blob[i] != '{' {
		return "", 0, 0, false
	}
	i++
	for {
		i = skipJSONWhitespace(blob, i)
		if i >= len(blob) {
			return "", 0, 0, false
		}
		if blob[i] == '}' {
			return "", 0, 0, false
		}
		if blob[i] == ',' {
			i++
			continue
		}
		key, _, _, next, ok := readJSONStringToken(blob, i)
		if !ok {
			return "", 0, 0, false
		}
		i = skipJSONWhitespace(blob, next)
		if i >= len(blob) || blob[i] != ':' {
			return "", 0, 0, false
		}
		i = skipJSONWhitespace(blob, i+1)
		if key == field {
			value, valueStart, valueEnd, _, ok = readJSONStringToken(blob, i)
			return value, valueStart, valueEnd, ok
		}
		next, ok = skipJSONValue(blob, i)
		if !ok {
			return "", 0, 0, false
		}
		i = next
	}
}

func skipJSONWhitespace(blob []byte, i int) int {
	for i < len(blob) {
		switch blob[i] {
		case ' ', '\n', '\r', '\t':
			i++
		default:
			return i
		}
	}
	return i
}

func readJSONStringToken(blob []byte, i int) (value string, start int, end int, next int, ok bool) {
	if i >= len(blob) || blob[i] != '"' {
		return "", 0, 0, 0, false
	}
	start = i
	escaped := false
	for i++; i < len(blob); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch blob[i] {
		case '\\':
			escaped = true
		case '"':
			end = i + 1
			if err := json.Unmarshal(blob[start:end], &value); err != nil {
				return "", 0, 0, 0, false
			}
			return value, start, end, end, true
		}
	}
	return "", 0, 0, 0, false
}

func skipJSONValue(blob []byte, i int) (int, bool) {
	i = skipJSONWhitespace(blob, i)
	if i >= len(blob) {
		return 0, false
	}
	if blob[i] == '"' {
		_, _, _, next, ok := readJSONStringToken(blob, i)
		return next, ok
	}
	if blob[i] != '{' && blob[i] != '[' {
		for i < len(blob) {
			switch blob[i] {
			case ',', '}', ']':
				return i, true
			case ' ', '\n', '\r', '\t':
				return skipJSONWhitespace(blob, i), true
			default:
				i++
			}
		}
		return 0, false
	}

	stack := []byte{blob[i]}
	escaped := false
	inString := false
	for i++; i < len(blob); i++ {
		c := blob[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) == 0 {
				return 0, false
			}
			open := stack[len(stack)-1]
			if (open == '{' && c != '}') || (open == '[' && c != ']') {
				return 0, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// RewriteModel replaces the "model" field in a JSON request body.
func RewriteModel(body []byte, newModel string) []byte {
	if len(body) == 0 {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	if _, ok := obj["model"]; !ok {
		return nil
	}
	obj["model"] = newModel
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return rewritten
}

func ConversationIDFromSSE(sample []byte) string {
	// Best-effort: scan lines for JSON fragments and grab conversation_id/conversation.
	for _, line := range bytes.Split(sample, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		if id := ConversationID(line); id != "" {
			return id
		}
	}
	return ""
}
