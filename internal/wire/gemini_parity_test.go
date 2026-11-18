package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

type geminiParityFunc struct {
	Codec         string `json:"codec"`
	Input         string `json:"input"`
	Format        string `json:"format,omitempty"`
	Model         string `json:"model,omitempty"`
	Request       string `json:"request,omitempty"`
	FunctionNames string `json:"function_names,omitempty"`
	Namespaced    string `json:"namespaced,omitempty"`
	Custom        string `json:"custom,omitempty"`
	Status        int    `json:"status,omitempty"`
	Output        string `json:"output"`
	Err           string `json:"err,omitempty"`
}

type geminiParityWrite struct {
	N   int    `json:"n"`
	Err string `json:"err,omitempty"`
}

type geminiParityStream struct {
	Format        string              `json:"format"`
	Model         string              `json:"model"`
	Request       string              `json:"request"`
	FunctionNames string              `json:"function_names"`
	Namespaced    string              `json:"namespaced"`
	Custom        string              `json:"custom"`
	Chunks        []string            `json:"chunks"`
	Writes        []geminiParityWrite `json:"writes"`
	Output        string              `json:"output"`
}

type geminiParityFixture struct {
	Funcs   []geminiParityFunc   `json:"funcs"`
	Streams []geminiParityStream `json:"streams"`
}

var (
	geminiParityUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	geminiParityTime = regexp.MustCompile(`"(created|created_at)":(\d{10})\b`)
)

func normalizeGeminiParityOutput(output string, start, end int64) string {
	ids := map[string]string{}
	output = geminiParityUUID.ReplaceAllStringFunc(output, func(id string) string {
		if ids[id] == "" {
			ids[id] = fmt.Sprintf("<uuid%d>", len(ids)+1)
		}
		return ids[id]
	})
	return geminiParityTime.ReplaceAllStringFunc(output, func(match string) string {
		parts := geminiParityTime.FindStringSubmatch(match)
		value, _ := strconv.ParseInt(parts[2], 10, 64)
		if value >= start-1 && value <= end+1 {
			return `"` + parts[1] + `":"<now>"`
		}
		return match
	})
}

func geminiParityFormat(t *testing.T, name string) RequestFormat {
	t.Helper()
	switch name {
	case "gemini":
		return FormatGemini
	case "openai_chat":
		return FormatOpenAI
	case "openai_responses":
		return FormatResponses
	case "anthropic_messages":
		return FormatClaude
	}
	t.Fatalf("unknown format %q", name)
	return 0
}

func decodeGeminiParityValue(t *testing.T, raw string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
}

func runGeminiParityFunc(t *testing.T, tc geminiParityFunc) geminiParityFunc {
	t.Helper()
	got := tc
	got.Output, got.Err = "", ""
	start := time.Now().Unix()
	var out []byte
	var err error
	switch tc.Codec {
	case "response":
		var request map[string]any
		var names map[string]string
		var namespaced, custom map[string]bool
		decodeGeminiParityValue(t, tc.Request, &request)
		decodeGeminiParityValue(t, tc.FunctionNames, &names)
		decodeGeminiParityValue(t, tc.Namespaced, &namespaced)
		decodeGeminiParityValue(t, tc.Custom, &custom)
		out, err = GeminiResponseToClient([]byte(tc.Input), geminiParityFormat(t, tc.Format), tc.Model, ResponsesToolContext{Request: request, FunctionNames: names, NamespacedTools: namespaced, CustomTools: custom})
	case "error":
		out = GeminiErrorToClient([]byte(tc.Input), geminiParityFormat(t, tc.Format), tc.Status)
	case "unwrap":
		out, err = UnwrapAntigravityResponse([]byte(tc.Input))
	default:
		t.Fatalf("unknown codec %q", tc.Codec)
	}
	got.Output = normalizeGeminiParityOutput(string(out), start, time.Now().Unix())
	if err != nil {
		// Go 1.27 aliases json.RawMessage to jsontext.Value in unmarshal type errors.
		got.Err = strings.ReplaceAll(err.Error(), "jsontext.Value", "json.RawMessage")
	}
	return got
}

func runGeminiParityStream(t *testing.T, tc geminiParityStream) geminiParityStream {
	t.Helper()
	got := tc
	got.Writes, got.Output = nil, ""
	start := time.Now().Unix()
	var out bytes.Buffer
	var request map[string]any
	var names map[string]string
	var namespaced, custom map[string]bool
	decodeGeminiParityValue(t, tc.Request, &request)
	decodeGeminiParityValue(t, tc.FunctionNames, &names)
	decodeGeminiParityValue(t, tc.Namespaced, &namespaced)
	decodeGeminiParityValue(t, tc.Custom, &custom)
	writer := NewGeminiToClientStream(&out, geminiParityFormat(t, tc.Format), tc.Model, ResponsesToolContext{Request: request, FunctionNames: names, NamespacedTools: namespaced, CustomTools: custom})
	for _, chunk := range tc.Chunks {
		n, err := writer.Write([]byte(chunk))
		write := geminiParityWrite{N: n}
		if err != nil {
			write.Err = strings.ReplaceAll(err.Error(), "jsontext.Value", "json.RawMessage")
		}
		got.Writes = append(got.Writes, write)
	}
	got.Output = normalizeGeminiParityOutput(out.String(), start, time.Now().Unix())
	return got
}

func TestGeminiCodecParity(t *testing.T) {
	data, err := os.ReadFile("testdata/gemini_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture geminiParityFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for i, tc := range fixture.Funcs {
		t.Run(fmt.Sprintf("%s/%d", tc.Codec, i), func(t *testing.T) {
			if got := runGeminiParityFunc(t, tc); got != tc {
				t.Fatalf("got  %+v\nwant %+v", got, tc)
			}
		})
	}
	for i, tc := range fixture.Streams {
		t.Run(fmt.Sprintf("stream-%s/%d", tc.Format, i), func(t *testing.T) {
			got, _ := json.Marshal(runGeminiParityStream(t, tc))
			want, _ := json.Marshal(tc)
			if !bytes.Equal(got, want) {
				t.Fatalf("got  %s\nwant %s", got, want)
			}
		})
	}
}
