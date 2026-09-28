# Brokered pre-create latency

The creation timeline distinguishes client startup, coordinator admission, provider
setup, and the actual VM request. `admission_complete` is emitted after the durable
reservation, provider preparation, access publication, and dispatch fence. It does
not mean the provider has received a create request yet. Compare it with
`provider_create_request` in `creationEvents`; these observations never authorize
ownership, readiness, or cleanup.

## Call path

The table describes ordinary Linux cold creates. Each HTTP read costs a network
round trip plus service processing; operation waits can take seconds. Per-call
latency is deployment-dependent and cannot be inferred from aggregate timeline
samples. Use provider diagnostics and a post-deployment probe to measure it.

| Stage | Awaited calls before VM dispatch | Ordering and cache policy |
| --- | --- | --- |
| Client | Local claim/slug reads and SSH key generation; AWS outbound-address GET to `checkip.amazonaws.com` when SSH CIDRs are unset; coordinator HTTP request | Address discovery has a five-second deadline. Fixed-ID locks are per lease, not a global create lock. |
| Admission | Replay/attempt and host-pin storage reads; optional promoted-image storage lookup; provider price lookup; reservation/usage/access scans; transactional reservation and wake writes; preparation/access publication and final dispatch-fence writes | Serialized state transitions preserve cost caps, exact ownership, cancellation, and cleanup. Storage retries reread exact committed state and are bounded to 500 ms. |
| AWS scope | Credential resolution if refresh is needed; STS `GetCallerIdentity` during preparation and again for the regional fixed-credential operation; target/access state publication | The operation must prove the persisted account with its own credential snapshot. Do not reuse an identity from a different snapshot. |
| AWS key | `DescribeKeyPairs`; if absent, `ImportKeyPair`, with ownership publication before an owned key needs cleanup | Key registration remains ahead of the remaining preparation. |
| AWS image | `DescribeImages` for stock images; no provider lookup for explicit/promoted AMIs | Runs alongside ingress, quota, and instance-type reads. Snapshot registration/availability and deregistration retain their existing ordering. |
| AWS ingress | `DescribeVpcs` or `DescribeSubnets`; `DescribeSecurityGroups`; conditional `CreateSecurityGroup` and propagation reads; ingress authorization/revocation and propagation retries | Existing parallel VPC/group reads remain; mutations keep the ingress owner and fresh access context. No cached ownership or ingress policy. |
| AWS quota/type | Service Quotas `GetServiceQuota`; EC2 `DescribeInstanceTypes` | Run alongside image/ingress. Quotas are already memoized per market within one create; every preparation settles before launch or error return. |
| GCP auth | OAuth service-account token POST, or trusted metadata token GET on a cache miss | Existing client-scoped expiry cache joins concurrent refreshes, refreshes before expiry, and retries failed refreshes on a later call. Scoped clients copy only completed tokens; credentials are not cached globally. |
| GCP firewall | GET exact firewall; conditional PUT or POST, followed by global operation `/wait` calls | A fresh managed firewall matching the complete effective policy skips PUT and its wait. Drift, incomplete policy, and insertion races retain reconciliation. No stale firewall-existence cache. |
| GCP image/network/key | No extra provider reads for ordinary configured image families, network/subnet references, zone/type selection, or SSH metadata | These references go into the instance insert. Resource identity, operation completion, and ownership checks after insert are unchanged. |
| Hetzner price | GET `/server_types` during admission | Existing bounded optional price lookup; failure retains configured/default pricing. |
| Hetzner key | GET `/ssh_keys?name=...`, then POST `/ssh_keys` for a missing per-lease key | Full paginated key inventory is deferred until a uniqueness conflict. Custom/shared names retain eager lookup; collision recovery still verifies key material and exact-name ownership. |
| Hetzner image/location/type | No extra remote preparation reads | Configured image/location and ordered type candidates go directly to server creation. |

Ordinary creates do not borrow a ready-pool entry. Explicit pool borrowing has its
own identity/reuse checks. Mutable inventory, cost usage, ownership claims, and
SSH access must not be cached across admission or cleanup boundaries merely to
shorten a create.

AWS diagnostics (`crabbox_aws_provisioning`) include phase and transport timings.
Overlapping step totals may exceed total wall time. The GCP fast path removes a
provider mutation and operation wait, rather than assuming an observed firewall
will remain unchanged for a TTL. This also detects externally changed policy on
the next create.

## Verification

Compare cold creates with the same provider, image, region, machine type, and
client revision. Keep TTL and idle limits short, run providers sequentially to
avoid local contention, collect `--timing-json` plus `creationEvents`, and verify
release of each exact lease. Changes to the coordinator need a deployed revision;
a new client alone cannot prove a coordinator latency improvement.

The regression tests gate independent AWS preparations, assert no launch or return
before they settle (including failures), exercise GCP policy drift after an
unchanged-policy fast path, and verify Hetzner cold-key and uniqueness paths.
