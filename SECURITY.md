# Security policy

## Supported versions

Only the latest release gets security fixes. Check yours with
`oai-plan-proxy version`.

## Reporting a vulnerability

Report it privately through
[GitHub private vulnerability reporting](https://github.com/klhq/oai-plan-proxy/security/advisories/new).
Do not open a public issue for a vulnerability.

Include the output of `oai-plan-proxy version`, what an attacker can do, and
the steps to reproduce it. Do not include your `credentials.json`, an export
file, or any token. You get a first answer within one week.

## What the proxy protects

The proxy holds a ChatGPT sign-in. Anyone with its refresh token can use your
ChatGPT plan until you sign out, so the design keeps that token in one place.

- **The API key.** The key is compared in constant time and never
  logged. `OAI_PLAN_PROXY_API_KEY_FILE` keeps it out of the environment,
  and a bad key file is reported by name, never by content.
- **Spending.** `OAI_PLAN_PROXY_MAX_CONCURRENT` caps the model requests in
  flight, so one runaway caller cannot spend the plan all at once.
- **Storage.** `credentials.json` and `host_id` live in
  `OAI_PLAN_PROXY_DATA_DIR`. The directory is created with mode `0700` and
  the files with mode `0600`. `export` moves the sign-in, so only one
  host holds the refresh token at a time. `export -` writes it to standard
  output for a pipe, and `export <file>` writes a file with mode `0600`.
- **Sign-in.** `login` uses the authorization code flow with PKCE and checks
  `state` on every callback, including a pasted one. A copied callback
  address is useless without the verifier, which never leaves the process.
- **Logs.** No log record contains an `Authorization` header value, an
  access, refresh, or ID token, the incoming API key, the account email or
  ID, or any request or response content. See [Logs](README.md#logs).
- **Sign-out.** `logout` revokes the refresh token at OpenAI before it
  deletes the stored credentials.

## What the proxy trusts

- **The network in front of it.** Without an API key, anyone who can reach
  the listener uses your plan. The default listen address is
  `127.0.0.1:18080`. On any address that is not loopback, `serve` refuses
  to start without `OAI_PLAN_PROXY_API_KEY` or
  `OAI_PLAN_PROXY_API_KEY_FILE`, unless you set
  `OAI_PLAN_PROXY_ALLOW_NO_KEY=1` to say the network already limits who
  can reach it. Other processes on the same host can still reach a
  loopback listener without a key.
- **The host.** Anyone who can read the data directory, or run as its owner,
  can take the sign-in.
- **OpenAI.** The proxy sends requests to the ChatGPT backend over TLS and
  takes its answers as they come.

## Verifying a release

Release archives and container images carry build provenance. Check a
download before you run it:

```bash
gh attestation verify oai-plan-proxy_${VERSION}_linux_amd64.tar.gz --repo klhq/oai-plan-proxy
gh attestation verify oci://ghcr.io/klhq/oai-plan-proxy:${VERSION} --repo klhq/oai-plan-proxy
gh attestation verify oci://docker.io/klhq/oai-plan-proxy:${VERSION} --repo klhq/oai-plan-proxy
```

Releases up to v0.1.2 have no attestations. Use `checksums.txt` for those.
Docker Hub images start at v0.1.7.
