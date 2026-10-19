package translate

import "strings"

// anthropicImageSourceFromURL builds an Anthropic image source from a URL.
func anthropicImageSourceFromURL(u string) any {
	if strings.HasPrefix(u, "data:") {
		parts := strings.SplitN(u, ",", 2)
		header := parts[0]
		data := ""
		if len(parts) == 2 {
			data = parts[1]
		}
		mediaType := "image/png"
		if i := strings.Index(header, ";"); i > 5 {
			mediaType = header[5:i]
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
