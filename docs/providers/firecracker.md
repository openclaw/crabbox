# Firecracker

`firecracker` is the direct Firecracker SSH lease provider surface for Linux KVM hosts.

Current PLAN-01 scope:

- provider registration and metadata
- `crabbox config show` support for `firecracker.*` settings
- flags and environment overrides for the Firecracker contract
- read-only `crabbox doctor --provider firecracker` host readiness checks

Lifecycle commands such as warmup, run, ssh, stop, and cleanup remain pending until the next Firecracker implementation plan.
