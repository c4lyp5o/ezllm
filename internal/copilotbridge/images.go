package copilotbridge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"strings"
)

const (
	maxImages          = 4
	maxImageBytes      = 4 << 20
	maxTotalImageBytes = 8 << 20
	maxImagePixels     = 20_000_000
)

// ImageAttachment is an in-memory CLI blob. No client URL or local file path
// reaches the CLI, and image data never becomes part of the text transcript.
type ImageAttachment struct {
	Type        string `json:"type"`
	Data        string `json:"data"`
	MIMEType    string `json:"mimeType"`
	DisplayName string `json:"displayName"`
}

func contentWithImages(raw json.RawMessage, attachments *[]ImageAttachment, total *int) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL    string `json:"url"`
			Detail string `json:"detail"`
		} `json:"image_url"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", fmt.Errorf("message content is neither a string nor a parts array")
	}
	var b strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text":
			b.WriteString(part.Text)
		case "image_url":
			if len(*attachments) >= maxImages {
				return "", fmt.Errorf("at most %d image attachments are supported", maxImages)
			}
			if part.ImageURL.Detail != "" && part.ImageURL.Detail != "auto" {
				return "", fmt.Errorf("image detail must be auto: the CLI controls image processing")
			}
			mime, data, ok := strings.Cut(part.ImageURL.URL, ",")
			if !ok {
				return "", fmt.Errorf("image_url must be an inline base64 PNG or JPEG data URL")
			}
			format := ""
			switch mime {
			case "data:image/png;base64":
				format = "png"
				mime = "image/png"
			case "data:image/jpeg;base64":
				format = "jpeg"
				mime = "image/jpeg"
			default:
				return "", fmt.Errorf("image_url must be an inline base64 PNG or JPEG data URL; remote URLs and file paths are not supported")
			}
			if len(data) > base64.StdEncoding.EncodedLen(maxImageBytes) {
				return "", fmt.Errorf("image attachment exceeds 4 MiB")
			}
			decoded, err := base64.StdEncoding.Strict().DecodeString(data)
			if err != nil || len(decoded) == 0 {
				return "", fmt.Errorf("image attachment has invalid base64 data")
			}
			if len(decoded) > maxImageBytes {
				return "", fmt.Errorf("image attachment exceeds 4 MiB")
			}
			if len(decoded) > maxTotalImageBytes-*total {
				return "", fmt.Errorf("image attachments exceed 8 MiB total")
			}
			cfg, actual, err := image.DecodeConfig(bytes.NewReader(decoded))
			if err != nil || actual != format {
				return "", fmt.Errorf("image bytes do not match a valid PNG or JPEG header")
			}
			if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
				return "", fmt.Errorf("image attachment exceeds 20 million pixels")
			}
			*total += len(decoded)
			name := fmt.Sprintf("image-%d.%s", len(*attachments)+1, format)
			*attachments = append(*attachments, ImageAttachment{Type: "blob", Data: base64.StdEncoding.EncodeToString(decoded), MIMEType: mime, DisplayName: name})
			fmt.Fprintf(&b, "\n[%s attached]\n", name)
		default:
			return "", fmt.Errorf("content part %q is not supported by this bridge", part.Type)
		}
	}
	return b.String(), nil
}

// validateImages checks the CLI's current advertised limits without inference.
// A model with unknown capabilities cannot silently receive a text-only prompt.
func (c *rpcConn) validateImages(model string, images []ImageAttachment) error {
	refuse := func(code, msg string) error { return &Failure{Code: code, Msg: "copilot-bridge: " + msg} }
	if model == "" || model == "auto" {
		return refuse("vision_unsupported", "image requests require an explicit vision-capable model")
	}
	var catalog struct {
		Models []struct {
			ID           string `json:"id"`
			Capabilities struct {
				Supports struct {
					Vision bool `json:"vision"`
				} `json:"supports"`
				Limits struct {
					Vision struct {
						Count int      `json:"max_prompt_images"`
						Size  int      `json:"max_prompt_image_size"`
						Types []string `json:"supported_media_types"`
					} `json:"vision"`
				} `json:"limits"`
			} `json:"capabilities"`
		} `json:"models"`
	}
	if err := c.call("models.list", map[string]any{}, &catalog); err != nil {
		return refuse("vision_unsupported", "could not verify image support from the CLI model catalog")
	}
	for _, m := range catalog.Models {
		if m.ID != model {
			continue
		}
		v := m.Capabilities.Limits.Vision
		if !m.Capabilities.Supports.Vision || v.Count <= 0 || v.Size <= 0 {
			return refuse("vision_unsupported", "selected model does not advertise usable image support")
		}
		if len(images) > v.Count {
			return refuse("invalid_image", fmt.Sprintf("selected model supports at most %d images", v.Count))
		}
		for _, a := range images {
			supported := false
			for _, mime := range v.Types {
				supported = supported || mime == a.MIMEType
			}
			if !supported {
				return refuse("invalid_image", "selected model does not advertise the supplied image format")
			}
			decoded, err := base64.StdEncoding.Strict().DecodeString(a.Data)
			if err != nil || len(decoded) > v.Size {
				return refuse("invalid_image", "image exceeds the selected model's byte limit")
			}
		}
		return nil
	}
	return refuse("vision_unsupported", "selected model is absent from the CLI vision catalog")
}
