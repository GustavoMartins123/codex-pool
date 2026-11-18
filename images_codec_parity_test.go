package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
)

type imagesParityCase struct {
	Codec          string `json:"codec"`
	Input          string `json:"input"`
	ContentType    string `json:"content_type,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	Count          int    `json:"count,omitempty"`
	Output         string `json:"output"`
	Detail         string `json:"detail,omitempty"`
	Err            string `json:"err,omitempty"`
}

func runImagesParityCase(t *testing.T, tc imagesParityCase) imagesParityCase {
	t.Helper()
	got := tc
	got.Output, got.Detail, got.Err = "", "", ""
	input := []byte(tc.Input)
	var out []byte
	var err error
	switch tc.Codec {
	case "images-count":
		out = []byte(strconv.Itoa(imagesGenerationCount(input)))
	case "images-set-count":
		out = setImagesGenerationCount(input, tc.Count)
	case "images-generation-request":
		out, got.Detail, err = translateImagesGenerationToResponses(input)
	case "images-edit-request":
		out, got.Detail, err = translateImagesEditToResponses(input, tc.ContentType)
	case "images-append-data":
		var data []any
		var created int64
		data, created, err = appendImagesGenerationData([]any{map[string]any{"b64_json": "prior"}}, input, tc.ResponseFormat)
		if err == nil {
			out, _ = json.Marshal(data)
			got.Detail = strconv.FormatInt(created, 10)
		}
	case "responses-to-images":
		out, err = translateResponsesToImagesGeneration(input, tc.ResponseFormat)
	default:
		t.Fatalf("unknown codec %q", tc.Codec)
	}
	got.Output = string(out)
	if err != nil {
		got.Err = err.Error()
	}
	return got
}

func TestImagesCodecParity(t *testing.T) {
	data, err := os.ReadFile("testdata/images_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []imagesParityCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%s/%d", tc.Codec, i), func(t *testing.T) {
			if got := runImagesParityCase(t, tc); got != tc {
				t.Fatalf("got  %+v\nwant %+v", got, tc)
			}
		})
	}
}
