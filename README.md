# oai-plan-proxy

A self-hosted proxy that serves an OpenAI-compatible API from your ChatGPT
plan. It signs in with the Sign in with ChatGPT flow for open-source tools,
keeps the sign-in alive, and serves `/v1/models`, `/v1/responses`, and
`/v1/chat/completions`.

> **Not an official OpenAI project.** This project is not affiliated with,
> endorsed by, or supported by OpenAI. Requests made through it count against
> your ChatGPT plan and fall under OpenAI's terms for that plan. Use it only
> with an account you are allowed to use this way.

## Requirements

- Linux or macOS. The credential lock uses `flock(2)`, so Windows is not
  supported.
- A ChatGPT plan that offers plan usage to open-source tools.

## Install

Pick one of three ways. Each release on the
[releases page](https://github.com/klhq/oai-plan-proxy/releases) has
binaries for Linux and macOS on amd64 and arm64, a `checksums.txt`, and a
multi-arch container image.

**Binary.** Download the archive for your platform and check it against
`checksums.txt` before you extract it:

```bash
VERSION=v0.1.2
PLATFORM=linux_amd64   # or linux_arm64, darwin_amd64, darwin_arm64
BASE=https://github.com/klhq/oai-plan-proxy/releases/download/$VERSION
curl -fsSLO "$BASE/oai-plan-proxy_${VERSION}_${PLATFORM}.tar.gz"
curl -fsSLO "$BASE/checksums.txt"
sha256sum --check --ignore-missing checksums.txt   # macOS: shasum -a 256 --check --ignore-missing checksums.txt
tar -xzf "oai-plan-proxy_${VERSION}_${PLATFORM}.tar.gz"
sudo install "oai-plan-proxy_${VERSION}_${PLATFORM}/oai-plan-proxy" /usr/local/bin/
```

**Container.** `ghcr.io/klhq/oai-plan-proxy`, tagged with each release and
`latest`. From v0.1.7 the same image is also on Docker Hub as
`klhq/oai-plan-proxy`. See [Container](#container).

**From source.** With Go 1.27 or later:

```bash
go install github.com/klhq/oai-plan-proxy/cmd/oai-plan-proxy@latest
```

Check what you installed:

```bash
oai-plan-proxy version
```

Include this line when you report a bug.

## Sign in

```bash
oai-plan-proxy login
```

`login` prints an authorization URL and waits for the browser to return to
`http://127.0.0.1:1455/auth/callback`. Open the URL, approve the consent
screen, and the command stores the result. `--port` changes the callback
port.

If the browser runs on another machine, the callback page fails to load
there. Copy the full address from that browser's address bar and paste it
into the waiting `login`. It completes the sign-in the same way. A line that
is not the callback address, or one from an older sign-in, gets a hint and
`login` keeps waiting.

The first sign-in registers this host with OpenAI and receives an issued
client ID (`oaiapp_...`). Later sign-ins reuse that ID, so `login` after a
dead sign-in does not register a new client.

Check the result:

```bash
oai-plan-proxy status
```

`status` prints the login state, account email, client ID, and access-token
expiry. It never prints a token.

### Sign in on a headless server

The callback listener binds `127.0.0.1` only. On a server without a browser,
use one of these paths.

**Paste the address (preferred).** Run `login` on the server over SSH, open
the printed URL in your laptop browser, approve, and paste the address the
browser lands on back into the SSH session:

```bash
ssh <host>
oai-plan-proxy login
```

The tokens never leave the server, and no port forwarding is needed.

**SSH tunnel.** Forward the callback port from your laptop, run `login` on
the server, and open the printed URL in the laptop browser:

```bash
ssh -L 1455:127.0.0.1:1455 <host>
oai-plan-proxy login
```

The browser redirect reaches the server through the tunnel. The tokens never
leave the server.

**Export and import.** Sign in on a machine with a browser and pipe the
sign-in straight into the server, so no file lands on either disk:

```bash
oai-plan-proxy login
oai-plan-proxy export - | ssh <host> 'oai-plan-proxy import -'
```

`export` moves the sign-in: once it is written out, the exporting machine
deletes its own copy without revoking it. The refresh token rotates on
every use, so two machines holding it would end up invalidating each other.
If the import fails, run `login` again. `export <file>` writes a file with
mode `0600` instead; it holds a live refresh token, so delete it after the
import. A failed export keeps the sign-in where it was. `import` refuses to
replace stored credentials unless you pass `--force`. Each host keeps its
own `host_id`.

## Container

The image is distroless and runs as a non-root user. It keeps its data in
the `/data` volume, listens on `0.0.0.0:18080`, and runs `serve` unless you
pass another command. Because that address is open, `serve` in the image
needs an API key. See [Serve](#serve).

```bash
docker volume create oai-plan-proxy-data
```

Sign in once, by either path:

**Paste the address.** Run `login` in a one-off interactive container, open
the printed URL in any browser, approve, and paste the address the browser
lands on into the container:

```bash
docker run --rm -it -v oai-plan-proxy-data:/data ghcr.io/klhq/oai-plan-proxy login
```

**Import.** Sign in on a laptop and pipe the export into the volume, so no
file lands on either machine. The laptop keeps no copy afterwards:

```bash
oai-plan-proxy login
oai-plan-proxy export - | ssh <host> docker run --rm -i -v oai-plan-proxy-data:/data ghcr.io/klhq/oai-plan-proxy import -
```

Then run it:

```bash
docker run -d --name oai-plan-proxy \
  -v oai-plan-proxy-data:/data \
  -e OAI_PLAN_PROXY_API_KEY=change-me \
  -p 127.0.0.1:18080:18080 \
  ghcr.io/klhq/oai-plan-proxy
```

With Docker or Compose secrets, mount the key as a file and point
`OAI_PLAN_PROXY_API_KEY_FILE` at it, so the key stays out of the
environment:

```yaml
services:
  oai-plan-proxy:
    image: ghcr.io/klhq/oai-plan-proxy
    volumes: [oai-plan-proxy-data:/data]
    environment:
      OAI_PLAN_PROXY_API_KEY_FILE: /run/secrets/oai_plan_proxy_key
    secrets: [oai_plan_proxy_key]
secrets:
  oai_plan_proxy_key:
    file: ./oai-plan-proxy-key.txt
volumes:
  oai-plan-proxy-data:
```

The image healthcheck runs `oai-plan-proxy healthcheck`, which exits 0 only
while `/healthz` answers 200. The container reports unhealthy while the
sign-in is missing or dead.

## Serve

```bash
OAI_PLAN_PROXY_API_KEY=change-me oai-plan-proxy serve
```

| Variable | Meaning | Default |
|---|---|---|
| `OAI_PLAN_PROXY_LISTEN` | Listen address | `127.0.0.1:18080` |
| `OAI_PLAN_PROXY_DATA_DIR` | Holds `credentials.json` and `host_id` | `os.UserConfigDir()/oai-plan-proxy` |
| `OAI_PLAN_PROXY_API_KEY` | Bearer key every `/v1` request must carry | unset (no check) |
| `OAI_PLAN_PROXY_API_KEY_FILE` | File that holds the key, instead of `OAI_PLAN_PROXY_API_KEY` | unset |
| `OAI_PLAN_PROXY_ALLOW_NO_KEY` | `1` to serve an open address without a key | unset |
| `OAI_PLAN_PROXY_MAX_CONCURRENT` | Model requests in flight at once; `0` means no limit | `0` |
| `OAI_PLAN_PROXY_USAGE_LIMIT_PAUSE` | How long to answer the usage limit at once when upstream gives no reset time, from `0` to `1h`; `0` turns the pause off | `30s` |
| `OAI_PLAN_PROXY_LOG_FORMAT` | `text` or `json` log records on standard error | `text` |

Clients send the key as `Authorization: Bearer <key>`. `serve` refuses to
start when the listen address is not loopback (`localhost`, `127.0.0.0/8`,
or `::1`) and no key is set, because anyone who can reach it would use your
plan. If the network in front of the proxy already limits who can reach
it, set `OAI_PLAN_PROXY_ALLOW_NO_KEY=1`; `serve` then starts and logs a
`no API key` warning. `OAI_PLAN_PROXY_API_KEY_FILE` trims whitespace around
the key, and setting both key variables is an error.

With `OAI_PLAN_PROXY_MAX_CONCURRENT=<n>`, a `POST /v1/responses` or
`/v1/chat/completions` request beyond the first n in flight gets 429 at
once, with `Retry-After: 1` and the code `concurrency_limit`. It carries no
`x-should-retry: false`, so a gateway may retry it. Use it to stop one
runaway caller from spending the plan.

`serve` refreshes the access token at least 60 seconds before it expires.
The refresh token rotates on every use. The new one is written to disk
before any request uses the new access token, and a file lock keeps two
processes on one data directory from refreshing at once.

`GET /healthz` answers 200 with `{"status":"ok","login":"valid"}` while the
sign-in works, and 503 with `"login":"missing"` or `"login":"dead"`
otherwise. It needs no API key. Point your container healthcheck at it.

### Logs

`serve` writes `log/slog` records to standard error. Set
`OAI_PLAN_PROXY_LOG_FORMAT=json` when a log collector reads them.

| `msg` | Level | Attributes |
|---|---|---|
| `listening` | info | `addr`, `upstream`, `strict`, `api_key_set`, `allow_no_key`, `max_concurrent`, `usage_limit_pause`, `login`, `version`, `commit` |
| `no API key` | warn | `addr`, `hint`; at startup with `OAI_PLAN_PROXY_ALLOW_NO_KEY=1` on an open address |
| `sign-in needed` | warn | `login`, `hint`; at startup when the sign-in is missing or dead |
| `request` | info, or warn for a status of 500 or more, a usage limit, a concurrency limit, or a stream error | see below |
| `token refreshed` | info | `reason` (`scheduled`, `expired`, `upstream_401`), `expires_at` |
| `token refresh failed` | warn | `reason`, `error`; the sign-in is kept and retried |
| `sign-in dead` | error | `reason`, `error`, `hint`; logged once, then sign in again |

Each request record carries `method`, `path`, `status`, and `duration_ms`.
For `/v1/responses` and `/v1/chat/completions` it adds `model`, `stream`,
and `dropped` when parameters were removed. A completed response adds
`input_tokens`, `output_tokens`, and `cached_tokens`. An upstream error adds
`upstream_status` and `error_code`. A failure after output started adds
`stream_error=true`, and a caller that hung up adds `canceled=true`. A
`/healthz` probe that answers 200 is not logged.

When upstream reports the plan's usage, the record adds it for the short
(`primary`) and long (`secondary`) windows:
`usage_primary_percent`, `usage_primary_window_minutes`, and
`usage_primary_resets_at` in RFC 3339, and the same three for
`usage_secondary`. Watch `usage_secondary_percent` to see the weekly limit
coming. A usage-limit answer that carries `Retry-After` adds
`retry_after_s`. A request the proxy answered without asking upstream, because
the limit was already known to hold, adds `paused=true`.

No record carries a token, the `Authorization` header, the API key, the
account email or ID, or any request or response content.

## Routes

| Route | Behavior |
|---|---|
| `GET /v1/models`, `GET /v1/models/{id}` | The plan's model catalog as an OpenAI model list. See below. |
| `POST /v1/responses` | Adjusted for plan usage, then forwarded. See below. |
| `POST /v1/chat/completions` | Translated to `/v1/responses` and back. See below. |
| `GET /healthz` | Liveness and login state. |
| Any other `/v1/*` path | OpenAI-shaped 404. |

An upstream 401 triggers one token refresh and one retry. Without a usable
sign-in, `/v1/*` answers 503 with a message to run `oai-plan-proxy login`.

### `/v1/models`

The plan answers `/v1/models` with a Codex model catalog, not an OpenAI model
list. The proxy fetches that catalog with the plan's access token and
answers with an OpenAI list: each model's `slug` becomes its `id`, and
models the catalog marks `hide` are left out. `GET /v1/models/{id}` looks the
id up in the same list and answers 404 `model_not_found` when it is not
there, because the plan token cannot read the upstream route for a single
model.

### `/v1/responses`

The plan route accepts a subset of the Responses API. The proxy edits only
the fields it has to and passes every other field through unchanged.

- `store` is forced to `false` and `stream` to `true`.
- These parameters are removed: `background`, `conversation`,
  `max_output_tokens`, `max_tool_calls`, `metadata`, `moderation`,
  `multi_agent`, `prompt`, `prompt_cache_retention`, `safety_identifier`,
  `temperature`, `top_logprobs`, `top_p`, `truncation`, `user`. The response
  header `x-oai-plan-proxy-dropped-params` lists the ones removed. With
  `serve --strict`, the request gets a 400 instead.
- A string `input` becomes a one-item message array. Input items with
  `role: system` are sent as `role: developer`.
- `previous_response_id` gets a 400. Send the full conversation instead.
- Hosted tools the route cannot run get a 400 that names the tool type:
  `image_generation`, `file_search`, `code_interpreter`, `computer`,
  `computer_use_preview`, `mcp`, and `tool_search`. `function`, `custom`, and
  `web_search` tools pass through.

With `"stream": true`, the caller receives the upstream events unchanged,
flushed one at a time. Without it, the caller receives the `response` object
from the final `response.completed` event as one JSON body.

A failure before the first output event returns a real HTTP error status,
never a 200 stream that carries an error. Once output has started, the
stream passes through as upstream sent it.

When the plan's usage limit is reached, the proxy answers 429 with
`x-should-retry: false`. A gateway in front of the proxy can then fall back
to another provider instead of retrying. Upstream reports the limit as the
error code `subscription_sharing_usage_limit_exceeded` or the error type
`usage_limit_reached`, over HTTP or as an event before any output. The
proxy answers both the same way.

The 429 carries `Retry-After` in seconds when the reset time is known:
from the error's `resets_at`, or else from the usage window that is spent.

Once upstream has reported the limit, the proxy stops asking. Every
`POST /v1/responses` and `/v1/chat/completions` after that gets the same 429
at once, with `Retry-After` set to the time left, until the reset time
upstream gave. Without a reset time, the pause lasts
`OAI_PLAN_PROXY_USAGE_LIMIT_PAUSE`, and a reset time further off than an
hour pauses for an hour. The next request after the pause goes to upstream,
and if the limit still holds, it starts the next pause. This saves the one
to two seconds each call otherwise spends learning the same answer, which a
gateway that falls back pays on every request. The limit is for the whole
plan, so the pause covers every model. `/healthz` and `GET /v1/models` are
not held back. `OAI_PLAN_PROXY_USAGE_LIMIT_PAUSE=0` turns the pause off.

### `/v1/chat/completions`

The plan serves only the Responses API, so the proxy translates Chat
Completions requests to it and translates the answer back. The request then
goes through the same `/v1/responses` handling described above.

What is translated:

- `system` and `developer` messages become developer messages in place.
  User text, `image_url` parts, and `file` parts with `file_data` become
  input parts. Assistant text and `refusal` parts stay in the history.
- Assistant `tool_calls` and `tool` messages become `function_call` and
  `function_call_output` items.
- `function` tools, `tool_choice`, `parallel_tool_calls`,
  `response_format`, `reasoning_effort`, and `service_tier` map to their
  Responses equivalents.
- The answer comes back as `chat.completion`, or as
  `chat.completion.chunk` events ending with `data: [DONE]` when `stream`
  is true. Text, refusals, `tool_calls`, `finish_reason`, and `usage` are
  filled in. `stream_options.include_usage` adds the usage chunk.

What is not:

- Fields without a Responses equivalent, such as `max_tokens`,
  `max_completion_tokens`, `stop`, `seed`, `logit_bias`, and the penalties,
  are removed and listed in `x-oai-plan-proxy-dropped-params`. With
  `serve --strict`, the request gets a 400 instead.
- Requests the proxy cannot answer as asked get a 400 that names the field:
  `n` above 1, `audio`, non-text `modalities`, `logprobs`, `top_logprobs`,
  the legacy `functions` and `function_call`, non-function tools,
  `input_audio` parts, and `file` parts that carry only a `file_id`.
- Reasoning is not returned.

A failure before the first chunk returns a real HTTP status, as on
`/v1/responses`. A failure after output has started ends the stream with
one `data: {"error": ...}` line and no `data: [DONE]`.

## Sign out

```bash
oai-plan-proxy logout
```

`logout` revokes the refresh token at OpenAI and deletes `credentials.json`.
If the revocation fails, the credentials stay so you can run `logout` again.
The `host_id` file stays in every case.

## Security

See [SECURITY.md](SECURITY.md) for how to report a vulnerability, what the
proxy protects, and how to verify a release.

## License

[MIT](LICENSE)
