package translate

import (
	"encoding/json"
)

// responsesContentBlocks parses a responses item's content into IR blocks.
func responsesContentBlocks(raw json.RawMessage, what string) ([]Block, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []Block{{Kind: BlockText, Text: s}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err == nil {
		var blocks []Block
		for _, p := range parts {
			var part struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL any    `json:"image_url"`
			}
			if err := json.Unmarshal(p, &part); err != nil {
				return nil, unsupported(what, "unreadable content part")
			}
			switch part.Type {
			case "input_text", "output_text", "text", "":
				if part.Text != "" {
					blocks = append(blocks, Block{Kind: BlockText, Text: part.Text})
				}
			case "input_image":
				url := ""
				switch v := part.ImageURL.(type) {
				case string:
					url = v
				case map[string]any:
					if u, ok := v["url"].(string); ok {
						url = u
					} else if u, ok := v["image_url"].(string); ok {
						url = u
					}
				}
				if url == "" {
					var alt struct {
						ImageURL string `json:"image_url"`
					}
					if json.Unmarshal(p, &alt) == nil {
						url = alt.ImageURL
					}
				}
				if url == "" {
					return nil, unsupported(what+".input_image", "missing image url")
				}
				blocks = append(blocks, Block{Kind: BlockImage, ImageURL: url})
			default:
				return nil, unsupported(what+"."+part.Type,
					"only text and image content is portable across the three wires")
			}
		}
		return blocks, nil
	}
	text, err := textContent(raw, what)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, nil
	}
	return []Block{{Kind: BlockText, Text: text}}, nil
}
