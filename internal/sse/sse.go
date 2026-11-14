// Package sse frames and parses Server-Sent Events streams. It is pure
// byte-level logic: no HTTP, logging or provider knowledge.
package sse

import (
	"bytes"
	"io"
)

func ExtractEventData(event []byte) []byte {
	_, data := ParseEvent(event)
	if len(data) == 0 {
		trimmed := bytes.TrimSpace(event)
		if len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '{') {
			data = trimmed
		} else {
			return nil
		}
	}
	return bytes.TrimSpace(data)
}

// Framer scans each byte once, retaining line state across writes. A CR
// ends a line immediately; a following LF belongs to that same line ending.
// Callers discard exactly advance bytes after each successful next call.
//
// When an event's blank-line terminator ends in a CR at the very end of the
// buffer and the pairing LF arrives in a later read, next returns the LF
// alone as a PSEUDO-EVENT: (nil, 1, true). The pseudo-event's byte belongs
// to the terminator of the previously returned event, so byte-relaying
// callers forward it verbatim, while suppression callers must inherit the
// previous event's drop decision (see sseInterceptWriter.Write).
type Framer struct {
	cursor    int
	lineStart int
	afterCR   bool
}

func (f *Framer) Next(buf []byte) (event []byte, advance int, ok bool) {
	for f.cursor < len(buf) {
		i := f.cursor
		c := buf[i]
		f.cursor++
		if f.afterCR {
			f.afterCR = false
			if c == '\n' {
				f.lineStart = f.cursor
				if i == 0 {
					// Preserve the LF of a split CRLF without delaying CR-only events.
					f.cursor = 0
					f.lineStart = 0
					return nil, 1, true
				}
				continue
			}
		}
		if c != '\r' && c != '\n' {
			continue
		}
		empty := i == f.lineStart
		f.lineStart = f.cursor
		f.afterCR = c == '\r'
		if !empty {
			continue
		}
		advance = f.cursor
		if f.afterCR && advance < len(buf) && buf[advance] == '\n' {
			advance++
			f.afterCR = false
		}
		f.cursor = 0
		f.lineStart = 0
		return buf[:i], advance, true
	}
	return nil, 0, false
}

// Write retains the first failure so later emissions cannot resume a broken stream.
func Write(w io.Writer, p []byte, first *error) {
	if *first != nil {
		return
	}
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	*first = err
}

func ParseEvent(event []byte) (string, []byte) {
	var eventType string
	var dataLines [][]byte
	for len(event) > 0 {
		end := bytes.IndexAny(event, "\r\n")
		if end < 0 {
			end = len(event)
		}
		line := event[:end]
		event = event[end:]
		if len(event) > 0 {
			cr := event[0] == '\r'
			event = event[1:]
			if cr && len(event) > 0 && event[0] == '\n' {
				event = event[1:]
			}
		}
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			eventType = string(bytes.TrimSpace(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			value := line[len("data:"):]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			dataLines = append(dataLines, value)
		}
	}
	return eventType, bytes.Join(dataLines, []byte("\n"))
}
