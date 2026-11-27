package translate

import (
	"encoding/base64"
	"net/url"
	"strings"
)

// anthropicImageSourceFromURL builds an Anthropic image source from a URL.
func anthropicImageSourceFromURL(u string) any {
	if strings.HasPrefix(u, "data:") {
		parts := strings.SplitN(u, ",", 2)
		header := parts[0]
		data := ""
		if len(parts) == 2 {
			data = parts[1]
		}
		// data:<type>[;param...][;base64],<data>. The type ends at the first
		// ';' or the comma; data without ;base64 is percent-encoded text,
		// which Anthropic's base64 source needs encoded.
		params := strings.Split(strings.TrimPrefix(header, "data:"), ";")
		mediaType := "image/png"
		if params[0] != "" {
			mediaType = params[0]
		}
		isBase64 := false
		for _, p := range params[1:] {
			if strings.EqualFold(p, "base64") {
				isBase64 = true
			}
		}
		if !isBase64 {
			if decoded, err := url.PathUnescape(data); err == nil {
				data = base64.StdEncoding.EncodeToString([]byte(decoded))
			}
		}
		return map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		}
	}
	return map[string]any{
		"type": "url",
		"url":  u,
	}
}
