# oai-plan-proxy

A self-hosted proxy that serves an OpenAI-compatible API from your ChatGPT
plan. It signs in with the Sign in with ChatGPT flow for open-source tools,
keeps the sign-in alive, and forwards `/v1/models` and `/v1/responses`.

> **Not an official OpenAI project.** This project is not affiliated with,
> endorsed by, or supported by OpenAI. Requests made through it count against
> your ChatGPT plan and fall under OpenAI's terms for that plan. Use it only
> with an account you are allowed to use this way.

## Requirements

- Go 1.27 or later.
- Linux or macOS. The credential lock uses `flock(2)`, so Windows is not
  supported.
- A ChatGPT plan that offers plan usage to open-source tools.

## Install

```bash
go install github.com/klhq/oai-plan-proxy/cmd/oai-plan-proxy@latest
```

## Sign in

```bash
oai-plan-proxy login
```

`login` prints an authorization URL and waits for the browser to return to
`http://127.0.0.1:1455/auth/callback`. Open the URL, approve the consent
screen, and the command stores the result. `--port` changes the callback
port.

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
use one of these two paths.

**SSH tunnel (preferred).** Forward the callback port from your laptop, run
`login` on the server, and open the printed URL in the laptop browser:

```bash
ssh -L 1455:127.0.0.1:1455 <host>
oai-plan-proxy login
```

The browser redirect reaches the server through the tunnel. The tokens never
leave the server.

**Export and import (fallback).** Sign in on a machine with a browser, copy
the credential file over SSH, and install it on the server:

```bash
oai-plan-proxy login
oai-plan-proxy export creds.json
scp creds.json <host>:
ssh <host> 'oai-plan-proxy import creds.json && rm creds.json'
rm creds.json
```

`export` writes the file with mode `0600`. It holds a live refresh token, so
delete every copy after the import. `import` refuses to replace stored
credentials unless you pass `--force`. Each host keeps its own `host_id`.

## Serve

```bash
OAI_PLAN_PROXY_API_KEY=change-me oai-plan-proxy serve
```

| Variable | Meaning | Default |
|---|---|---|
| `OAI_PLAN_PROXY_LISTEN` | Listen address | `127.0.0.1:18080` |
| `OAI_PLAN_PROXY_DATA_DIR` | Holds `credentials.json` and `host_id` | `os.UserConfigDir()/oai-plan-proxy` |
| `OAI_PLAN_PROXY_API_KEY` | Bearer key every `/v1` request must carry | unset (no check) |

Set `OAI_PLAN_PROXY_API_KEY` whenever the listener is reachable by anything
other than you. Clients send it as `Authorization: Bearer <key>`.

`serve` refreshes the access token at least 60 seconds before it expires.
The refresh token rotates on every use. The new one is written to disk
before any request uses the new access token, and a file lock keeps two
processes on one data directory from refreshing at once.

`GET /healthz` answers 200 with `{"status":"ok","login":"valid"}` while the
sign-in works, and 503 with `"login":"missing"` or `"login":"dead"`
otherwise. It needs no API key. Point your container healthcheck at it.

## Routes

| Route | Behavior |
|---|---|
| `GET /v1/models`, `GET /v1/models/{id}` | Forwarded with the plan's access token. |
| `POST /v1/responses` | Adjusted for plan usage, then forwarded. See below. |
| `GET /healthz` | Liveness and login state. |
| Any other `/v1/*` path | OpenAI-shaped 404. |

An upstream 401 triggers one token refresh and one retry. Without a usable
sign-in, `/v1/*` answers 503 with a message to run `oai-plan-proxy login`.

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

When the plan's usage limit is reached
(`subscription_sharing_usage_limit_exceeded`), the proxy answers 429 with
`x-should-retry: false`. A gateway in front of the proxy can then fall back
to another provider instead of retrying.

## Sign out

```bash
oai-plan-proxy logout
```

`logout` revokes the refresh token at OpenAI and deletes `credentials.json`.
If the revocation fails, the credentials stay so you can run `logout` again.
The `host_id` file stays in every case.

## License

[MIT](LICENSE)
