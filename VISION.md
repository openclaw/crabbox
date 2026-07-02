# Crabbox Vision

Crabbox makes remote software execution disposable without making ownership or cleanup ambiguous. Core behavior stays provider-neutral; provider adapters own provider-specific lifecycle rules.

## Lifecycle Safety

- Destructive and reuse operations require verified ownership bound to the exact provider, resource, and claim. Labels, names, and IDs alone are not ownership proof.
- Legacy or external adoption is explicit and conflict-safe. Adoption may bind an unclaimed resource, but must never silently retarget an already-bound claim.
- Provider lifecycle paths fail closed when ownership checks or inventory/list operations fail. Claims preserve enough non-secret provider, resource, endpoint, account, and connection metadata to route and guarantee cleanup without persisting credentials.
- Funded or remote providers require real create, use, and destroy proof with zero residue before merge, including cleanup after partial failure.
