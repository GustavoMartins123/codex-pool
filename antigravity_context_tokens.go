package main

import (
	"encoding/base64"
	"encoding/json"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"strings"
)

const (
	antigravityImageTileTokens     = 258
	antigravityImageFallbackTokens = 3000
	antigravityPDFFallbackTokens   = 25800
	antigravityImageOverheadTokens = 32
	antigravityJSONMapOverhead     = 8
	antigravityJSONArrayOverhead   = 4
	antigravityJSONScalarTokens    = 2
)

func estimateAntigravityRequestTokens(path string, body []byte) int {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return 0
	}
	model := stringValue(root["model"])
	if model == "" {
		model = antigravityModelFromGeminiPath(path)
	}
	if model != "" {
		if prepared, err := prepareAntigravityRequest(path, body, model, "token-estimate", ""); err == nil {
			if tokens := estimateAntigravitySerializedTokens(prepared.Body); tokens > 0 {
				return tokens
			}
		}
	}
	object := contextRequestObject(root)
	messages := normalizeConversationMessages(detectContextWireFormat(path, object), object)
	if tokens := estimateContextTokens(messages); tokens > 0 {
		return tokens
	}
	return estimateAntigravitySerializedTokens(body)
}

func estimateAntigravitySerializedTokens(raw []byte) int {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return estimateAntigravityTextTokens(string(raw))
	}
	return estimateAntigravityWireValue(value, "")
}

func estimateAntigravityWireValue(value any, key string) int {
	switch typed := value.(type) {
	case map[string]any:
		total := antigravityJSONMapOverhead
		mimeType := strings.ToLower(stringValue(typed["mimeType"]))
		imageData := ""
		hasImageData := false
		if strings.HasPrefix(mimeType, "image/") {
			if data, ok := typed["data"].(string); ok {
				imageData = data
				hasImageData = true
			} else if _, ok := typed["fileUri"]; ok {
				total += antigravityImageFallbackTokens + antigravityImageOverheadTokens
			}
		} else if strings.HasPrefix(mimeType, "application/pdf") {
			total += antigravityPDFFallbackTokens + antigravityImageOverheadTokens
		}
		for childKey, child := range typed {
			total += estimateAntigravityTextTokens(childKey)
			if hasImageData && childKey == "data" {
				total += estimateAntigravityImageTokens(imageData)
				continue
			}
			total += estimateAntigravityWireValue(child, childKey)
		}
		return total
	case []any:
		total := antigravityJSONArrayOverhead
		for _, child := range typed {
			total += estimateAntigravityWireValue(child, key)
		}
		return total
	case string:
		return estimateAntigravityTextTokens(typed)
	case bool:
		return 1
	case float64:
		return antigravityJSONScalarTokens
	case nil:
		return 1
	default:
		return estimateAntigravityTextTokens(key)
	}
}

func estimateAntigravityTextTokens(text string) int {
	if text == "" {
		return 0
	}
	ascii := 0
	nonASCII := 0
	for _, character := range text {
		if character <= 127 {
			ascii++
		} else {
			nonASCII++
		}
	}
	return int(math.Ceil(float64(ascii)/3.0 + float64(nonASCII)*1.5))
}

func estimateAntigravityImageTokens(data string) int {
	encoded := data
	if strings.HasPrefix(encoded, "data:") {
		if comma := strings.IndexByte(encoded, ','); comma >= 0 {
			encoded = encoded[comma+1:]
		}
	}
	var config image.Config
	var err error
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		config, _, err = image.DecodeConfig(base64.NewDecoder(encoding, strings.NewReader(encoded)))
		if err == nil && config.Width > 0 && config.Height > 0 {
			break
		}
	}
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return antigravityImageFallbackTokens + antigravityImageOverheadTokens
	}
	widthTiles := (config.Width + 767) / 768
	heightTiles := (config.Height + 767) / 768
	if widthTiles < 1 {
		widthTiles = 1
	}
	if heightTiles < 1 {
		heightTiles = 1
	}
	return widthTiles*heightTiles*antigravityImageTileTokens + antigravityImageOverheadTokens
}
