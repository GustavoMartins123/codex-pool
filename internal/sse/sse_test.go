package sse

import (
	"testing"
)

func TestParseSSEEventJoinsMultilineData(t *testing.T) {
	eventType, data := ParseEvent([]byte("event: example\r\ndata: first\r\ndata: second\r\n"))
	if eventType != "example" {
		t.Fatalf("event type = %q", eventType)
	}
	if string(data) != "first\nsecond" {
		t.Fatalf("data = %q", data)
	}
}
