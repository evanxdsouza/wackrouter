# Wackrouter

A small, slightly unhinged AI proxy with a web UI. Put all your provider API keys in one place, then point every app at one address with a token you can revoke.

One Go binary, standard library only, UI embedded. Around 1,200 lines in total.

## Run

    go build -o wackrouter . && ./wackrouter
    # or: docker build -t wackrouter . && docker run -p 8787:8787 -v wackrouter:/data wackrouter

Presets include OpenAI, Anthropic, Gemini, NVIDIA NIM, OpenRouter, Groq and more.

Open http://localhost:8787. On first start the admin password is printed in the log (set `WACKROUTER_PASSWORD` to choose your own).

Then: **Providers** → add one with a key → **Access** → create a token → use it.

    curl http://localhost:8787/v1/chat/completions \
      -H "Authorization: Bearer wr-…" -H "Content-Type: application/json" \
      -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"Hi"}]}'

## How it routes

- `model` is `provider/model`. The prefix picks the provider and is stripped before forwarding. Aliases (Settings) map short names like `fast` to a full model. No prefix means the default provider.
- Several keys on one provider rotate round-robin. A 429, 401, 403 or 5xx moves the request to the next key before anything reaches your app.
- `GET /v1/models` lists every provider's models (cached 5 minutes).
- `/p/<provider>/…` passes a request through untouched, for native APIs. For the Anthropic SDK or Claude Code: `ANTHROPIC_BASE_URL=http://host:8787/p/anthropic`, `ANTHROPIC_API_KEY=<wackrouter token>`.
- Streaming works. Token counts in Activity are read from the provider's usage fields.

## Custom providers and headers

Any OpenAI-compatible service works: pick **Custom** in Add provider, or edit an existing one.

- **Auth style:** Bearer (default), Anthropic `x-api-key`, or **Custom header** (header name plus optional prefix, e.g. `api-key` for Azure-style APIs).
- **Extra headers:** sent with every request to that provider (`HTTP-Referer`, project ids, gateway keys). Values are encrypted at rest like keys and win over the client's own header of the same name.
- Everything else the client sends, including `User-Agent`, is forwarded as-is.
- **AgentRouter** is a preset (`agentrouter/…`, base `https://agentrouter.org/v1`). It uses `x-api-key` on `/v1/messages` and Bearer on `/chat/completions`. Reported to accept only known coding-agent clients, so point Claude Code, Codex or similar at it through Wackrouter (`ANTHROPIC_BASE_URL=https://your-host/p/agentrouter`) and their own headers reach it unchanged. Plain curl or the Playground may be rejected with "unauthorized client detected".

## Drains (Logflare and webhooks)

Settings → **Drains** ships one event per request, batched every couple of seconds, one retry, never blocking the request path.

- **Logflare:** `POST {url}/api/logs?source=<uuid>` (or `source_name=`), `X-API-KEY` header, body `{"batch":[{"message","timestamp","metadata":{"wackrouter":{…}}}]}`. Set the URL to your own host for self-hosted Logflare.
- **Webhook:** a JSON array of flat events, with an optional `Authorization` header value.
- Fields: who, provider, model, path, key label, status, latency_ms, tokens_in, tokens_out, stream, error. Prompts, replies and keys are never sent.

## Config (environment)

| Variable | Default | Purpose |
|---|---|---|
| `WACKROUTER_ADDR` / `PORT` | `:8787` | Listen address |
| `WACKROUTER_DATA` | `./data` | `wackrouter.json`, `master.key`, `requests.jsonl` |
| `WACKROUTER_PASSWORD` | generated | Admin password |
| `WACKROUTER_SECRET` | generated into `master.key` | Encryption secret for stored keys |

## Security notes

- Provider keys are AES-256-GCM encrypted in `wackrouter.json`; the UI only ever shows the last 4 characters. Client tokens are stored as SHA-256 hashes and shown once.
- `master.key` sits next to the data by default, which protects against a leaked JSON file but not a leaked folder. Set `WACKROUTER_SECRET` from your platform's secrets to keep them apart.
- Put it behind HTTPS if it leaves localhost (session cookie turns `Secure` when it sees TLS or `X-Forwarded-Proto: https`).
- Request bodies are not logged, only metadata.
