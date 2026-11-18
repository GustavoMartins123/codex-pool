package main

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

type geminiRequestParityCase struct {
	Path            string `json:"path"`
	Body            string `json:"body"`
	PublicModel     string `json:"public_model"`
	UpstreamModel   string `json:"upstream_model"`
	ConversationID  string `json:"conversation_id"`
	Output          string `json:"output"`
	Format          string `json:"format"`
	ClientStream    bool   `json:"client_stream"`
	Operation       string `json:"operation"`
	Request         string `json:"request"`
	FunctionNames   string `json:"function_names"`
	NamespacedTools string `json:"namespaced_tools"`
	CustomTools     string `json:"custom_tools"`
	Err             string `json:"err"`
}

func runGeminiRequestParity(t *testing.T, tc geminiRequestParityCase) geminiRequestParityCase {
	t.Helper()
	prepare := func() (antigravityPreparedRequest, error) {
		return prepareGeminiNativeRequest(tc.Path, []byte(tc.Body), tc.PublicModel, tc.UpstreamModel, tc.ConversationID, func(gemini map[string]any) map[string]any {
			return map[string]any{"model": tc.UpstreamModel, "request": gemini}
		})
	}
	prepared, err := prepare()
	again, _ := prepare()
	got := tc
	got.Output = normalizeGeminiRequestSessionID(t, prepared.Body, again.Body)
	got.Format = prepared.Format.String()
	got.ClientStream = prepared.ClientStream
	got.Operation = prepared.Operation
	got.Request = geminiRequestParityJSON(t, prepared.ResponsesRequest)
	got.FunctionNames = geminiRequestParityJSON(t, prepared.ResponsesFunctionNames)
	got.NamespacedTools = geminiRequestParityJSON(t, prepared.ResponsesNamespacedTools)
	got.CustomTools = geminiRequestParityJSON(t, prepared.ResponsesCustomTools)
	got.Err = ""
	if err != nil {
		got.Err = err.Error()
	}
	return got
}

// normalizeGeminiRequestSessionID masks the session ID only when it is seeded
// from a random UUID, which two identical calls disagree on.
func normalizeGeminiRequestSessionID(t *testing.T, first, second []byte) string {
	t.Helper()
	if len(first) == 0 {
		return ""
	}
	var a, b map[string]any
	if err := json.Unmarshal(first, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &b); err != nil {
		t.Fatal(err)
	}
	requestA, _ := a["request"].(map[string]any)
	requestB, _ := b["request"].(map[string]any)
	if requestA != nil && requestB != nil && requestA["sessionId"] != requestB["sessionId"] {
		requestA["sessionId"] = "<random>"
	}
	return geminiRequestParityJSON(t, a)
}

func geminiRequestParityJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestGeminiRequestTranslationParity(t *testing.T) {
	data, err := os.ReadFile("testdata/gemini_request_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []geminiRequestParityCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%s/%d", tc.Path, i), func(t *testing.T) {
			if got := runGeminiRequestParity(t, tc); got != tc {
				t.Fatalf("got  %+v\nwant %+v", got, tc)
			}
		})
	}
}
