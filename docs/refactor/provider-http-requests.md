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

## Linode page data

Linode's five resource-list methods select their result types directly. One
provider-local typed collector replaces the endpoint-to-type switch and its
union of unrelated slices. `withPage` still supplies numbered pages of 500.

The collector retains the anonymous raw metadata envelope and decodes `data`
separately. Invalid metadata and invalid resource data therefore keep their
existing error boundaries. Later failures return previously accumulated pages,
but never partially decoded current-page data. Ordering, duplicates and nil
results are preserved. Empty data and the reported result count do not stop
traversal; only the existing page-count rule does.

## Compact JSON requests

`shared.NewCompactJSONRequest` owns the separate Marshal-based envelope used by
Hostinger, Vast, Morph, Upstash Box, Railway and Cloud Run Sandbox. It preserves
compact bytes without an Encoder newline, default HTML escaping, context,
content length and body replay. A nil interface leaves the body absent; a
typed-nil value still produces `null`.

URL construction and context lifetime remain caller-owned. Morph still reports
its URL parsing failures before encoding. Cloud Run still establishes and
cancels its timeout outside the constructor, and a typed-nil map remains a JSON
body. Headers, transport, retries and response handling do not move.

Stage-specific error labels, fallible checks between encoding and construction,
signed payloads, distinct empty-reader contracts and streaming readers remain
separate. The two named constructors describe different wire contracts, not a
configurable provider-client framework.
