package provider

import "strings"

// cloudflareFreeRaw is the operator-supplied list of Workers AI models shown
// free in the Cloudflare dashboard (2026-10-20), as "model id|dashboard task".
// "Free" here is Cloudflare's account-level free tier, which carries no marker
// in the model id — hence a table, not IsFreeModelID's suffix rule. The task
// label is kept verbatim: catalogs do not advertise per-service capability
// (docs/services-design.md), so this is the only place the class is known.
const cloudflareFreeRaw = `
	@cf/pipecat-ai/smart-turn-v2|Dumb Pipe
	@cf/openai/gpt-oss-120b|Text Generation
	@cf/baai/bge-m3|Text Embeddings
	@cf/huggingface/distilbert-sst-2-int8|Text Classification
	@cf/google/gemma-2b-it-lora|Text Generation
	@cf/black-forest-labs/flux-2-klein-9b|Text-to-Image
	@cf/meta/llama-3.2-3b-instruct|Text Generation
	@cf/meta/llama-guard-3-8b|Text Generation
	@cf/qwen/qwen3-embedding-0.6b|Text Embeddings
	@cf/myshell-ai/melotts|Text-to-Speech
	@cf/mistral/mistral-7b-instruct-v0.2-lora|Text Generation
	@cf/deepgram/aura-2-es|Text-to-Speech
	@cf/openai/whisper|Automatic Speech Recognition
	@cf/utter-project/eurollm-9b-it|Text Generation
	@cf/pfnet/plamo-embedding-1b|Text Embeddings
	@cf/llava-hf/llava-1.5-7b-hf|Image-to-Text
	@cf/deepseek-ai/deepseek-r1-distill-qwen-32b|Text Generation
	@cf/runwayml/stable-diffusion-v1-5-inpainting|Text-to-Image
	@cf/deepgram/flux|Automatic Speech Recognition
	@cf/deepgram/nova-3|Automatic Speech Recognition
	@cf/black-forest-labs/flux-1-schnell|Text-to-Image
	@cf/meta/llama-3.1-8b-instruct-fp8|Text Generation
	@cf/meta/llama-3.2-1b-instruct|Text Generation
	@cf/cloudflare/clef|Text Generation
	@cf/zai-org/glm-4.7-flash|Text Generation
	@cf/microsoft/resnet-50|Image Classification
	@cf/bytedance/stable-diffusion-xl-lightning|Text-to-Image
	@cf/meta-llama/llama-2-7b-chat-hf-lora|Text Generation
	@cf/meta/llama-3.3-70b-instruct-fp8-fast|Text Generation
	@cf/ibm-granite/granite-4.0-h-micro|Text Generation
	@cf/lykon/dreamshaper-8-lcm|Text-to-Image
	@cf/leonardo/phoenix-1.0|Text-to-Image
	@cf/stabilityai/stable-diffusion-xl-base-1.0|Text-to-Image
	@cf/cloudflare/clairic|Text Generation
	@cf/meta/m2m100-1.2b|Translation
	@cf/ai4bharat/indictrans2-en-indic-1B|Translation
	@cf/black-forest-labs/flux-2-klein-4b|Text-to-Image
	@cf/baai/bge-small-en-v1.5|Text Embeddings
	@cf/swiss-ai/apertus-v1.5-8b|Text Generation
	@cf/cloudflare/clef-omni|Text Generation
	@cf/qwen/qwen2.5-coder-32b-instruct|Text Generation
	@cf/nvidia/nemotron-3-120b-a12b|Text Generation
	@cf/baai/bge-base-en-v1.5|Text Embeddings
	@cf/aisingapore/gemma-sea-lion-v4-27b-it|Text Generation
	@cf/qwen/qwen3-30b-a3b-fp8|Text Generation
	@cf/black-forest-labs/flux-2-dev|Text-to-Image
	@cf/google/gemma-7b-it-lora|Text Generation
	@cf/google/gemma-4-26b-a4b-it|Text Generation
	@cf/mistralai/mistral-small-3.1-24b-instruct|Text Generation
	@cf/meta/llama-3.2-11b-vision-instruct|Text Generation
	@cf/daily-co/phonellm-mini-alpha-2|Text Generation
	@cf/qwen/qwen3.8-27b|Text Generation
	@cf/openai/whisper-tiny-en|Automatic Speech Recognition
	@cf/openai/whisper-large-v3-turbo|Automatic Speech Recognition
	@cf/deepgram/aura-1|Text-to-Speech
	@cf/openai/gpt-oss-20b|Text Generation
	@cf/google/embeddinggemma-300m|Text Embeddings
	@cf/baai/bge-reranker-base|Text Classification
	@cf/moondream/moondream3.1-9B-A2B|Image-to-Text
	@cf/leonardo/lucid-origin|Text-to-Image
	@cf/meta/llama-4-scout-17b-16e-instruct|Text Generation
	@cf/cloudflare/clef-flash|Text Generation
	@cf/qwen/qwq-32b|Text Generation
	@cf/baai/bge-large-en-v1.5|Text Embeddings
	@cf/deepgram/aura-2-en|Text-to-Speech
`

// cloudflareTextTask is the dashboard label of the chat-capable class.
const cloudflareTextTask = "Text Generation"

var cloudflareFree = func() map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(cloudflareFreeRaw, "\n") {
		if id, task, ok := strings.Cut(strings.TrimSpace(l), "|"); ok {
			m[id] = task
		}
	}
	return m
}()

// IsCloudflareWorkersAI reports whether an account is the Cloudflare Workers
// AI preset (host-scoped exactly like isCloudflareAI: kind + https + exact
// host + /ai/v1 suffix), from the two fields a catalog row join carries.
func IsCloudflareWorkersAI(kind, baseURL string) bool {
	return isCloudflareAI(Account{Kind: Kind(kind), BaseURL: baseURL})
}

// CloudflareFreeTask returns the dashboard task of a known-free Workers AI
// model id ("Text Generation", "Text Embeddings", ...). ok=false when the id is
// not on the free list.
func CloudflareFreeTask(modelID string) (task string, ok bool) {
	task, ok = cloudflareFree[modelID]
	return
}

// IsCloudflareFreeChatModel reports whether a model on a Workers AI account is
// free AND chat-capable — the only class a combo hop can serve. Embedding,
// image, audio, translation and classifier models stay on the list as data
// (CloudflareFreeTask) but never join a chat combo: they would fail every chat
// request the combo tried them on.
func IsCloudflareFreeChatModel(kind, baseURL, modelID string) bool {
	if !IsCloudflareWorkersAI(kind, baseURL) {
		return false
	}
	task, ok := CloudflareFreeTask(modelID)
	return ok && task == cloudflareTextTask
}
