# LLM Provider Setup

tinyRAG is provider-agnostic: anything that speaks the OpenAI `/v1/chat/completions`
and `/v1/embeddings` API works, local or cloud. Servers exposing the native
Ollama `/api` endpoints are supported as well. This page collects setup
notes for every provider preset in the web UI's provider switcher
(`#llmSwitcher` in [index.html](../internal/app/web/index.html)), plus how to wire up
something that isn't in the list.

Two independent endpoints can be configured — `chat_base` and `embed_base`
(Settings → LLM Backend, or `-url` on first run). Most local runners serve
both from the same base URL; for cloud chat-only providers (e.g. Anthropic,
Groq) you typically keep a separate embedding backend (a local model, or
OpenAI) since not every provider offers an embeddings endpoint.

The **Inference-Schnittstelle** setting controls the wire profile:

- `auto` (default) infers local Ollama conventions and retries the other
  profile only when a route is missing;
- `openai` uses `/v1/models`, `/v1/chat/completions` and `/v1/embeddings`;
- `ollama` uses `/api/tags`, `/api/chat` and `/api/embed`, with a fallback to
  the older one-input `/api/embeddings` route.

## Local runners

### LM Studio
- Download from [lmstudio.ai](https://lmstudio.ai/).
- Load a chat model and an embedding model (e.g. `nomic-embed-text`) in the
  "Local Server" tab, then start the server.
- Default base URL: `http://localhost:1234`.
- Fully OpenAI-compatible, including `/v1/models` for auto-discovery.

### Ollama
- Install: `curl -fsSL https://ollama.ai/install.sh | sh` (Linux/macOS) or
  the installer from [ollama.ai](https://ollama.ai/) (Windows/macOS).
- Pull models: `ollama pull llama3.1` and `ollama pull nomic-embed-text`.
- Default base URL: `http://localhost:11434`.
- Ollama's OpenAI-compatibility layer lives under `/v1` — tinyRAG accounts
  for this automatically. Selecting `Ollama nativ` uses `/api` directly and
  is useful for deployments that do not expose the compatibility layer.

### GopherLLM and RustyLLM

Both names are supported as provider hints in the UI. The normal profile is
**OpenAI-kompatibel**: point tinyRAG at the server's base URL and use
`/v1/models`, `/v1/chat/completions` and `/v1/embeddings`. This also keeps the
setup compatible with other Go/Rust inference applications that expose the
same contract. The switcher uses `http://localhost:8091` for the bundled demo
default; change the URL when the server listens elsewhere.

### llama.cpp (`llama-server`)
- Build or download `llama-server` from
  [ggml-org/llama.cpp](https://github.com/ggml-org/llama.cpp).
- Run: `llama-server -m model.gguf --embedding -m embed-model.gguf --port 8080`
  (embeddings require `--embedding` and a model that supports pooling).
- Default base URL: `http://localhost:8080`.
- Note: this port is shared with LocalAI and some `llmster` setups — the
  auto-detected provider hint is a best-effort guess; pick "llama.cpp"
  explicitly in the switcher if the guess is wrong.

### vLLM
- Install: `pip install vllm`.
- Run: `vllm serve <model> --port 8000` (vLLM exposes an OpenAI-compatible
  server by default).
- Default base URL: `http://localhost:8000`.
- Best for GPU-backed high-throughput serving; embeddings support depends on
  the model and vLLM version.

### text-generation-webui (oobabooga)
- Install per the
  [project's instructions](https://github.com/oobabooga/text-generation-webui).
- Enable the OpenAI-compatible extension (`--extensions openai` or via the UI).
- Default base URL: `http://localhost:5000`.

### KoboldCpp
- Download from [KoboldCpp releases](https://github.com/LostRuins/koboldcpp).
- Run with `--port 5001`; KoboldCpp exposes an OpenAI-compatible endpoint
  alongside its native API.
- Default base URL: `http://localhost:5001`.

### Jan
- Download from [jan.ai](https://jan.ai/).
- Enable the local API server in Jan's settings (Settings → Advanced →
  Local API Server).
- Default base URL: `http://localhost:1337`.

### LocalAI
- Run via Docker: `docker run -p 8080:8080 localai/localai`.
- Fully OpenAI-compatible, supports chat, embeddings, and more.
- Default base URL: `http://localhost:8080` (shares the port convention with
  llama.cpp — pick "LocalAI" explicitly in the switcher).

## Demo mode: embedded GopherLLM (no external tool at all)

For trying tinyRAG with **zero setup** — no LM Studio, no Ollama, no
llama.cpp, nothing to install or start separately — it can run
[GopherLLM](https://github.com/SimonWaldherr/GopherLLM), a pure-Go GGUF
inference runtime, **in-process**. This is a demo/evaluation convenience,
not a production inference path: no chat UI, no tool calling, no request
concurrency tuning — whatever tiny model is loaded answers every request,
on CPU, at whatever speed pure-Go inference gets you (noticeably slower
than llama.cpp/LM Studio's hand-tuned C++ kernels, especially for anything
bigger than a ~1B-parameter model).

Not part of the default build — it pulls in GopherLLM's full GGUF/tokenizer/
SIMD stack, which most deployments don't need. Build with:

```bash
go build -tags demo_llm -o bin/tinyrag ./cmd/tinyrag
```

Then either point at a specific `.gguf` file, or let it auto-discover one:

```bash
# Explicit model file
./bin/tinyrag -demo-llm-model /path/to/model.Q4_K_M.gguf

# Auto-discover: scans GopherLLM's default model directory
# ($RUSTY_LLM_MODEL_DIR, or LM Studio's community model cache under $HOME)
# for the first supported, non-projector GGUF file it finds.
./bin/tinyrag -demo-llm-model auto

# Change the local port the embedded server listens on (default 127.0.0.1:8091)
./bin/tinyrag -demo-llm-model auto -demo-llm-addr 127.0.0.1:9000
```

On startup tinyRAG loads the model, starts GopherLLM's OpenAI-compatible
server as a background goroutine on `-demo-llm-addr`, waits for it to
answer, then points `chat_base`/`embed_base` at it automatically — no
manual Settings step needed. GopherLLM does not auto-download models; you
need a `.gguf` file already on disk. Good demo-sized picks (small enough to
run acceptably on CPU):

| Model | Approx. size (Q4_K_M) | Notes |
|---|---|---|
| Qwen2.5-0.5B-Instruct | ~400 MB | Good instruction-following for its size |
| SmolLM2-360M-Instruct | ~250 MB | Smaller/faster, weaker reasoning |
| SmolLM2-135M-Instruct | ~100 MB | Fastest, best for quick UI demos |

Supported architectures: llama, llama2, llama3, mistral, mistral3, qwen2,
gemma, gemma2, gpt-oss. See `demo_llm.go`
(`startEmbeddedDemoLLM`/`resolveDemoLLMModel`) for the integration; the
important gotcha: **`go mod tidy` without `-tags=demo_llm` will drop the
GopherLLM requirement from go.mod** since it can't see the build-tag-gated
import — run `go mod tidy -tags=demo_llm` instead when tidying this module.

## Cloud providers

| Provider | Base URL | Notes |
|---|---|---|
| OpenAI | `https://api.openai.com` | Set the API key in Settings or `OPENAI_API_KEY` env var. |
| Anthropic | `https://api.anthropic.com` | Chat only — pair with a local or OpenAI embedding backend. |
| Google Gemini | `https://generativelanguage.googleapis.com` | Uses the OpenAI-compatibility endpoint Google provides under this host. |
| Mistral AI | `https://api.mistral.ai` | Offers both chat and embedding models. |
| Groq | `https://api.groq.com/openai` | Chat only, very low latency. |
| DeepSeek | `https://api.deepseek.com` | Chat only. |
| Together AI | `https://api.together.xyz` | Hosts many open-weight models; check model-specific embedding support. |
| xAI (Grok) | `https://api.x.ai` | Chat only. |
| Cohere | `https://api.cohere.ai` | Offers a dedicated embeddings API alongside chat. |
| Perplexity | `https://api.perplexity.ai` | Chat only, includes web-grounded models. |
| OpenRouter | `https://openrouter.ai/api` | Routes to many upstream models through one key. |

All cloud providers need an API key — set it in Settings → LLM Backend
("OpenAI API Key" field applies to whichever base URL is active) or via the
`OPENAI_API_KEY` environment variable, which is used as a fallback whenever
no key is stored in `config/settings.json`.

## Using a provider that isn't listed

Pick "Custom..." in the provider switcher (or just fill in Settings → LLM
Backend directly): any server implementing `/v1/chat/completions` and
`/v1/embeddings` works, including self-hosted proxies or a provider added
after this document was written. For Ollama-native deployments select the
`ollama` profile. Enter the base URL (without a trailing `/v1` — tinyRAG
normalizes that), click "Test & Load Models" to populate the model dropdowns,
then save.

## How auto-detection works

- `providerHintFromURL` ([llm_discovery.go](../internal/app/llm_discovery.go)) maps a
  base URL to a human-readable provider name, first by hostname (cloud
  providers) and then by port (local runners) — used to pre-select the
  provider switcher and label the workspace pill.
- `localLLMCandidates` lists the local ports above; `maybePreferOfflineLLM`
  probes them in order on startup if the configured endpoint doesn't
  respond, and switches to the first reachable one automatically.
- `POST /api/llm/list-models` probes a given base URL live from the web UI
  (used by the provider switcher and Settings' "Test & Load Models" button).
