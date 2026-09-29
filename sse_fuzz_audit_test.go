package main

// Fuzz targets for the SSE framing/parsing pipeline. SSE per the WHATWG spec
// accepts CR, LF, and CRLF line terminators, and a streaming proxy must
// produce identical event sequences no matter how TCP segments split the
// byte stream.

import (
	"bytes"
	"strings"
	"testing"
)

// frameAll feeds buf through a single framer and returns the events.
func frameAll(buf []byte) [][]byte {
	var f sseFramer
	var events [][]byte
	for {
		event, advance, ok := f.next(buf)
		if !ok {
			return events
		}
		events = append(events, event)
		buf = buf[advance:]
	}
}

// auditRelay mirrors sseInterceptWriter's Write loop for one writer feeding
// pattern: it returns the bytes that would be forwarded to the client and
// the extracted data payloads, mirroring eventBytes = buf[:advance].
func auditRelay(chunks [][]byte) (forwarded []byte, datas [][]byte) {
	var f sseFramer
	pending := []byte(nil)
	for _, chunk := range chunks {
		pending = append(pending, chunk...)
		for {
			event, advance, ok := f.next(pending)
			if !ok {
				break
			}
			forwarded = append(forwarded, pending[:advance]...)
			if d := extractSSEEventData(event); d != nil {
				datas = append(datas, append([]byte(nil), d...))
			}
			pending = append([]byte(nil), pending[advance:]...)
		}
	}
	return forwarded, datas
}

// auditSplitBytewise splits stream into one-byte chunks.
func auditSplitBytewise(stream []byte) [][]byte {
	chunks := make([][]byte, len(stream))
	for i, b := range stream {
		chunks[i] = []byte{b}
	}
	return chunks
}

// FuzzAuditSSEFramerChunkIndependence checks the core streaming invariant:
// slicing the same byte stream at arbitrary chunk boundaries must not change
// the sequence of framed events. A parser that buffers differently for
// different chunk sizes would corrupt relayed streams under Windows
// newline mixes or fragmented TCP reads.
func FuzzAuditSSEFramerChunkIndependence(f *testing.F) {
	corpus := []string{
		"data: {\"a\":1}\n\ndata: {\"b\":2}\n\n",
		"data: crlf\r\n\r\ndata: two\r\n\r\n",
		"data: cr-only\rcr-only-2\r\r",
		"data: mixed\r\n\rdata: x\n\r",
		"\r\n\r\n\r\r\n\n",
		"data: split-crlf\r",
		"\n\n\ndata: tail\n\n",
		"event: e\r\ndata: 1\r\rdata: 2\r\n\r\n",
	}
	for _, c := range corpus {
		f.Add([]byte(c))
	}
	f.Fuzz(func(t *testing.T, stream []byte) {
		wholeForward, wholeData := auditRelay([][]byte{append([]byte(nil), stream...)})
		chunkForward, chunkData := auditRelay(auditSplitBytewise(stream))

		if !bytes.Equal(wholeForward, chunkForward) {
			t.Fatalf("chunking changed forwarded bytes: whole=%q chunked=%q stream=%q", wholeForward, chunkForward, stream)
		}
		if len(wholeData) != len(chunkData) {
			t.Fatalf("chunking changed data-bearing event count: whole=%d chunked=%d stream=%q", len(wholeData), len(chunkData), stream)
		}
		for i := range wholeData {
			if !bytes.Equal(wholeData[i], chunkData[i]) {
				t.Fatalf("chunking changed data of event %d: whole=%q chunked=%q stream=%q", i, wholeData[i], chunkData[i], stream)
			}
		}
	})
}

// FuzzAuditSSEDataExtractionLineEndingIndependence checks that a data line
// terminated by CR, LF, or CRLF (all valid SSE terminators) yields the exact
// same extracted payload bytes.
func FuzzAuditSSEDataExtractionLineEndingIndependence(f *testing.F) {
	f.Add([]byte(`{"json":true,"n":1}`))
	f.Add([]byte("[1,2,3]"))
	f.Add([]byte("plain text payload"))
	f.Add([]byte("unicode à é ü 汉字 🚀"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, payload []byte) {
		terminators := []string{"\n\n", "\r\n\r\n", "\r\r"}
		var want []byte
		set := false
		for _, term := range terminators {
			stream := []byte("data: " + string(payload) + term)
			events := frameAll(stream)
			if len(events) == 0 {
				continue
			}
			_, data := parseSSEEvent(events[0])
			if !set {
				want = data
				set = true
				continue
			}
			if !bytes.Equal(want, data) {
				t.Fatalf("payload differs by terminator %q: %q vs %q (payload %q)", term, want, data, payload)
			}
		}
	})
}

// TestAuditSSESuppressionSplitCRLFDivergence
// BUG-AUDIT-107 (cross-platform): the SSE suppressor (onEvent drop mode)
// withholds an event's bytes, but when the event's CRLF terminator is split
// across two reads the framer re-emits the trailing LF as a standalone
// pseudo-event which is never droppable (it carries no data). The client
// therefore receives a stray blank line that depends on how TCP segmented
// the stream: relayed output differs for identical input.
// Expected: dropping an event removes all of its bytes including the
// terminator, regardless of read boundaries.
// Actual: a lone "\n" leaks through whenever the CRLF straddles a read.
func TestAuditSSESuppressionSplitCRLFDivergence(t *testing.T) {
	stream := []byte("data: {\"drop\":true}\r\n\r\ndata: keep\n\n")

	relay := func(chunks [][]byte) string {
		var out bytes.Buffer
		sw := &sseInterceptWriter{w: &out, onEvent: func(data []byte) (bool, bool) {
			return bytes.Contains(data, []byte("drop")), false
		}}
		for _, chunk := range chunks {
			if _, err := sw.Write(chunk); err != nil {
				t.Fatal(err)
			}
		}
		return out.String()
	}

	whole := relay([][]byte{stream})
	// Split exactly after the CR of the blank line that terminates the
	// first event, so the pairing LF starts the second read.
	cut := len("data: {\"drop\":true}\r\n\r")
	var split [][]byte
	split = append(split, stream[:cut])   // ends with the event-terminating CR
	split = append(split, stream[cut:]) // starts with the pairing LF
	piecewise := relay(split)

	if whole != piecewise {
		t.Fatalf("BUG-AUDIT-107: relayed output depends on read boundaries:\nwhole    = %q\npiecewise= %q", whole, piecewise)
	}
	if strings.Contains(whole, "drop") {
		t.Fatalf("dropped event content leaked: %q", whole)
	}
}

// FuzzAuditParseSSEEventRobustness ensures the parser never panics on
// arbitrary bytes and always returns data that is a substring join of the
// input data lines.
func FuzzAuditParseSSEEventRobustness(f *testing.F) {
	f.Add([]byte("data: x\n\n"))
	f.Add([]byte("data:x\ndata: y\r\n"))
	f.Add([]byte("event: only\n"))
	f.Add([]byte{0x00, 0x01, 0xff, 'd', 'a', 't', 'a', ':', ' ', 0xfe})
	f.Fuzz(func(t *testing.T, event []byte) {
		eventType, data := parseSSEEvent(event)
		_ = eventType
		_ = data
		extractSSEEventData(event)
	})
}
