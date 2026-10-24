package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Research-only archive prototype. Production continues to use session_events.
type historyStudyCodec struct {
	name  string
	zip   func([]byte) ([]byte, error)
	unzip func([]byte, int) ([]byte, error)
}

func historyStudyCodecs(tb testing.TB) []historyStudyCodec {
	tb.Helper()
	var codecs []historyStudyCodec
	for _, level := range []int{gzip.BestSpeed, gzip.DefaultCompression} {
		codecs = append(codecs, historyStudyCodec{
			name: fmt.Sprintf("gzip-%d", level),
			zip: func(raw []byte) ([]byte, error) {
				var out bytes.Buffer
				writer, err := gzip.NewWriterLevel(&out, level)
				if err != nil {
					return nil, err
				}
				if _, err := writer.Write(raw); err != nil {
					return nil, err
				}
				if err := writer.Close(); err != nil {
					return nil, err
				}
				return out.Bytes(), nil
			},
			unzip: func(encoded []byte, size int) ([]byte, error) {
				reader, err := gzip.NewReader(bytes.NewReader(encoded))
				if err != nil {
					return nil, err
				}
				defer reader.Close()
				return io.ReadAll(io.LimitReader(reader, int64(size)+1))
			},
		})
	}
	for _, level := range []zstd.EncoderLevel{zstd.SpeedFastest, zstd.SpeedDefault} {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<20), zstd.WithEncoderCRC(true))
		if err != nil {
			tb.Fatal(err)
		}
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20))
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { encoder.Close(); decoder.Close() })
		codecs = append(codecs, historyStudyCodec{
			name: "zstd-" + level.String(),
			zip:  func(raw []byte) ([]byte, error) { return encoder.EncodeAll(raw, nil), nil },
			unzip: func(encoded []byte, size int) ([]byte, error) {
				return decoder.DecodeAll(encoded, make([]byte, 0, size))
			},
		})
	}
	return codecs
}

type historyStudyBlock struct {
	First        uint64 `json:"first"`
	Last         uint64 `json:"last"`
	RawBytes     int    `json:"raw_bytes"`
	EncodedBytes int    `json:"encoded_bytes"`
	Offset       int64  `json:"offset"`
	SHA256       string `json:"sha256"`
	RawSHA256    string `json:"raw_sha256"`
	data         []byte
}

type historyStudyPreview struct {
	TurnID string `json:"turn_id"`
	Role   string `json:"role"`
	Text   string `json:"text"`
}

type historyStudyManifest struct {
	Version   int                   `json:"version"`
	Principal string                `json:"principal_id"`
	Session   string                `json:"session_id"`
	Codec     string                `json:"codec"`
	Cursor    uint64                `json:"cursor"`
	Blocks    []historyStudyBlock   `json:"blocks"`
	Preview   []historyStudyPreview `json:"preview"`
}

func historyStudyDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func historyStudyBuildArchive(events []sessionEvent, codec historyStudyCodec, blockBytes int) (historyStudyManifest, error) {
	manifest := historyStudyManifest{Version: 1, Principal: "synthetic-principal", Session: "synthetic-session", Codec: codec.name, Preview: historyStudyPreviews(events)}
	var raw bytes.Buffer
	var first uint64
	var offset int64
	flush := func(last uint64) error {
		if raw.Len() == 0 {
			return nil
		}
		encoded, err := codec.zip(raw.Bytes())
		if err != nil {
			return err
		}
		manifest.Blocks = append(manifest.Blocks, historyStudyBlock{First: first, Last: last, RawBytes: raw.Len(), EncodedBytes: len(encoded), Offset: offset,
			SHA256: historyStudyDigest(encoded), RawSHA256: historyStudyDigest(raw.Bytes()), data: encoded})
		offset += int64(len(encoded))
		raw.Reset()
		first = 0
		return nil
	}
	for i, event := range events {
		if event.ID != uint64(i+1) {
			return manifest, errors.New("non-contiguous input")
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return manifest, err
		}
		if raw.Len() > 0 && raw.Len()+len(encoded)+1 > blockBytes {
			if err := flush(event.ID - 1); err != nil {
				return manifest, err
			}
		}
		if first == 0 {
			first = event.ID
		}
		raw.Write(encoded)
		raw.WriteByte('\n')
		manifest.Cursor = event.ID
	}
	return manifest, flush(manifest.Cursor)
}

func historyStudyDecodeBlock(block historyStudyBlock, data []byte, codec historyStudyCodec) ([]sessionEvent, error) {
	if len(data) != block.EncodedBytes || historyStudyDigest(data) != block.SHA256 {
		return nil, errors.New("archive checksum mismatch")
	}
	raw, err := codec.unzip(data, block.RawBytes)
	if err != nil {
		return nil, err
	}
	if len(raw) != block.RawBytes || historyStudyDigest(raw) != block.RawSHA256 {
		return nil, errors.New("archive content mismatch")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var events []sessionEvent
	for expected := block.First; expected <= block.Last; expected++ {
		var event sessionEvent
		if err := decoder.Decode(&event); err != nil {
			return nil, err
		}
		if event.ID != expected || event.Type == "" || !json.Valid(event.Data) {
			return nil, errors.New("invalid archive event")
		}
		events = append(events, event)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("unexpected archive trailing content")
	}
	return events, nil
}

func historyStudyRestore(manifest historyStudyManifest, principal string, codec historyStudyCodec) ([]sessionEvent, error) {
	if manifest.Version != 1 || manifest.Principal != principal || manifest.Codec != codec.name {
		return nil, errors.New("invalid archive identity or format")
	}
	var events []sessionEvent
	var cursor uint64
	for _, block := range manifest.Blocks {
		if block.First != cursor+1 {
			return nil, errors.New("archive block range missing")
		}
		decoded, err := historyStudyDecodeBlock(block, block.data, codec)
		if err != nil {
			return nil, err
		}
		events = append(events, decoded...)
		cursor = block.Last
	}
	if cursor != manifest.Cursor {
		return nil, errors.New("archive incomplete")
	}
	return events, nil
}

func historyStudyPreviews(events []sessionEvent) []historyStudyPreview {
	var previews []historyStudyPreview
	for _, event := range events {
		var content struct {
			TurnID  string `json:"turn_id"`
			Input   string `json:"input"`
			Kind    string `json:"kind"`
			Content string `json:"content"`
		}
		if event.Type != "turn.input" && event.Type != "turn.delta" {
			continue
		}
		if err := json.Unmarshal(event.Data, &content); err != nil {
			panic(err)
		}
		role, text := "user", content.Input
		if event.Type == "turn.delta" {
			if content.Kind != "text" {
				continue
			}
			role, text = "assistant", content.Content
		}
		last := len(previews) - 1
		if last >= 0 && previews[last].TurnID == content.TurnID && previews[last].Role == role {
			previews[last].Text = string([]rune(previews[last].Text + text)[:min(512, len([]rune(previews[last].Text+text)))])
		} else {
			textRunes := []rune(text)
			previews = append(previews, historyStudyPreview{content.TurnID, role, string(textRunes[:min(512, len(textRunes))])})
			if len(previews) > 3 {
				previews = previews[len(previews)-3:]
			}
		}
	}
	return previews
}

func historyStudyFixture(count int, entropy bool) []sessionEvent {
	random := rand.New(rand.NewPCG(42, 2026))
	words := strings.Fields("review implement validate session history account runner archive restore pagination command workspace result request response test database transaction durable event stream ownership integrity example generated synthetic document changes verified successful pending completed")
	events := make([]sessionEvent, 0, count)
	for i := range count {
		turn := fmt.Sprintf("turn-%08d", i/100)
		var content strings.Builder
		if entropy || i%5 == 0 {
			raw := make([]byte, 384)
			for k := range raw {
				raw[k] = byte(random.Uint64())
			}
			content.WriteString(base64.RawStdEncoding.EncodeToString(raw))
		} else {
			for content.Len() < 512 {
				fmt.Fprintf(&content, "%s ", words[random.IntN(len(words))])
			}
		}
		kind := "text"
		if i%100 < 15 {
			kind = "thinking"
		} else if i%100 < 20 {
			kind = "tool_call"
		} else if i%100 < 40 {
			kind = "tool_result"
		}
		event := newSessionEvent("turn.delta", map[string]any{"turn_id": turn, "kind": kind, "content": content.String()})
		if i%100 == 0 {
			event = newSessionEvent("turn.input", map[string]any{"turn_id": turn, "input": content.String()})
		}
		event.ID = uint64(i + 1)
		events = append(events, event)
	}
	return events
}

func TestHistoryArchiveStudyIsLosslessAndRejectsCorruptionAndForeignOwners(t *testing.T) {
	events := historyStudyFixture(1200, false)
	for _, codec := range historyStudyCodecs(t) {
		t.Run(codec.name, func(t *testing.T) {
			archive, err := historyStudyBuildArchive(events, codec, 256<<10)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := historyStudyRestore(archive, archive.Principal, codec)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(events)
			after, _ := json.Marshal(restored)
			if !bytes.Equal(before, after) {
				t.Fatal("archive changed transcript")
			}
			if _, err := historyStudyRestore(archive, "foreign", codec); err == nil {
				t.Fatal("foreign principal accepted")
			}
			bad := archive
			bad.Blocks = append([]historyStudyBlock{}, archive.Blocks...)
			bad.Blocks[0].data = bytes.Clone(bad.Blocks[0].data)
			bad.Blocks[0].data[0] ^= 1
			if _, err := historyStudyRestore(bad, bad.Principal, codec); err == nil {
				t.Fatal("corrupt block accepted")
			}
			bad = archive
			bad.Cursor++
			if _, err := historyStudyRestore(bad, bad.Principal, codec); err == nil {
				t.Fatal("missing tail accepted")
			}
		})
	}
}

func TestHistoryArchiveStudyPreviewGroupsDeltasAndKeepsLastLogicalMessages(t *testing.T) {
	events := []sessionEvent{
		newSessionEvent("turn.input", map[string]any{"turn_id": "one", "input": "first question"}),
		newSessionEvent("turn.delta", map[string]any{"turn_id": "one", "kind": "thinking", "content": "private analysis"}),
		newSessionEvent("turn.delta", map[string]any{"turn_id": "one", "kind": "text", "content": "first "}),
		newSessionEvent("turn.delta", map[string]any{"turn_id": "one", "kind": "text", "content": "answer"}),
		newSessionEvent("turn.delta", map[string]any{"turn_id": "one", "kind": "tool_result", "content": "tool output"}),
		newSessionEvent("turn.input", map[string]any{"turn_id": "two", "input": "second question"}),
		newSessionEvent("turn.delta", map[string]any{"turn_id": "two", "kind": "text", "content": strings.Repeat("界", 600)}),
	}
	preview := historyStudyPreviews(events)
	if len(preview) != 3 || preview[0].Text != "first answer" || preview[1].Text != "second question" ||
		preview[2].Role != "assistant" || len([]rune(preview[2].Text)) != 512 {
		t.Fatalf("invalid message previews: %+v", preview)
	}
}

func BenchmarkHistoryArchiveCodec(b *testing.B) {
	events := historyStudyFixture(10000, false)
	for _, codec := range historyStudyCodecs(b) {
		b.Run(codec.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := historyStudyBuildArchive(events, codec, 256<<10); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
