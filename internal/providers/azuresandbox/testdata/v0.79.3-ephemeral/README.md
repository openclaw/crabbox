# v0.79.3 ephemeral claim compatibility fixture

These are synthetic provider-test records, not customer state or Azure credentials.

- Producer: Crabbox `v0.79.3`, commit `bbaae2ece3df9eaaba93ec1ddbd67aa5f2296f9b`.
- Writer path: that release's `fixtureBackend` -> `backend.acquire` -> durable fixed-claim writer.
- Acquisition settings: the release's default test settings (four-hour TTL, 15-minute idle timeout, public `ubuntu` disk); repository root `/example/repo`.
- Clock: Go `testing/synctest` at its default epoch, keeping the stored finite window reproducible.
- `claim.json` is copied byte-for-byte from the old claim store after acquisition.
- `settings.json` and `sandbox.json` are JSON encodings of the old provider settings and observed fake resource. Random attempt/revision identifiers are harmless fixture identities.

`TestRetainedUpgradePreservesV0793EphemeralClaim` loads these unchanged bytes through the new production claim reader, replays without creating a resource or changing the deadline, observes status, and retires the exact ephemeral claim while keeping journal v1. The test also checks that the Azure settings retain the same serialized meaning.

This proves stored-shape and adapter replay compatibility; it does not qualify the live Azure retained-disk service contract.
