# Provider JSON requests

DigitalOcean, Vultr and Linode use `shared.NewJSONRequest` for the same
context-bound request envelope. It encodes non-nil bodies with `json.Encoder`
before calling `http.NewRequestWithContext`. The helper performs no I/O.

A nil interface means no body; a typed-nil value still encodes as JSON.
Encoder's trailing newline, escaping and error precedence are intentional.
Using its buffer directly preserves content length and `GetBody` replay.
Callers supply their existing concatenated URL; the helper does not join paths.

Provider adapters retain headers, credentials, transport, retries and response
handling. DigitalOcean and Vultr always set JSON content type; Linode sets it
only for a non-nil body. Vultr constructs a fresh envelope inside every attempt,
so a retry re-encodes the body. This is not a common cloud client or pagination
policy: those behaviors remain provider-owned.
