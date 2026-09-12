# Provider JSON requests

Providers using default-Encoder JSON HTTP bodies share `shared.NewJSONRequest`
for their context-bound request envelope. It encodes non-nil bodies with
`json.Encoder` before calling `http.NewRequestWithContext`. The helper performs
no I/O.

DigitalOcean, Vultr, Linode, Lambda, Cloudflare, Cloudflare Dynamic Workers,
Azure Dynamic Sessions, E2B, CubeSandbox and Sprites use this owner. Buffered
JSON request construction stays separate from response streaming. Dynamic
Workers still constructs a fresh request inside each existing attempt.

URL and query composition remain adapter-owned. The existing E2B, CubeSandbox
and Sprites query-bearing calls have no body; their body-bearing calls have no
query. Their local URL construction retains the behavior of those callers.
Orgo's unescaped JSON, OVH's trimmed signed payloads, Marshal-based bodies and
binary or framed streams keep their separate encoding contracts.

A nil interface means no body; a typed-nil value still encodes as JSON.
Encoder's trailing newline, escaping and error precedence are intentional.
Using its buffer directly preserves content length and `GetBody` replay.
Callers supply their existing concatenated URL; the helper does not join paths.

Provider adapters retain headers, credentials, transport, retries and response
handling. DigitalOcean and Vultr always set JSON content type; Linode sets it
only for a non-nil body. Vultr constructs a fresh envelope inside every attempt,
so a retry re-encodes the body. This is not a common cloud client or pagination
policy: those behaviors remain provider-owned.
