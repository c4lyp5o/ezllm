package translate

import "strings"

// chatContentFromBlocks renders a turn's blocks as a chat content value:
// a plain string when only text is present (preserves the legacy shape), or
// an array of parts when images are involved.
func chatContentFromBlocks(blocks []Block) any {
	var textParts []string
	var imageParts []map[string]any
	for _, b := range blocks {
		switch b.Kind {
		case BlockText:
			if b.Text != "" {
				textParts = append(textParts, b.Text)
			}
		case BlockImage:
			imageParts = append(imageParts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": b.ImageURL},
			})
		}
	}
	if len(imageParts) == 0 {
		return strings.Join(textParts, "\n")
	}
	var parts []map[string]any
	if len(textParts) > 0 {
		parts = append(parts, map[string]any{"type": "text", "text": strings.Join(textParts, "\n")})
	}
	parts = append(parts, imageParts...)
	return parts
}
