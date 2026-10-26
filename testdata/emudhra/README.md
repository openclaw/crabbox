# Native runtime distrust fixture

`old-published-roots.pem` is the deliberately affected public CA input used by
this incident campaign, SHA256
`714d457d580922dbf1d0be8bd35ba236a842b50b0072ae791582a19adef772a5`.
It tests compromised-key rejection independently of removing roots from defaults.
It contains public certificates, not private keys, and is only supplied to
isolated validation processes.

`scripts/native-node-bootstrap-proof.mjs` exercises the real platform bootstrap
and the executable it selects. The CI steps are limited to the finite
`security/emudhra-node-release` campaign branch. Healthy older macOS/Windows
runtimes are intentionally retained by the existing bootstrap contract; the
proof records that behavior separately from fresh install and broken-runtime
recovery. No image or release is published.

The repository attributes preserve LF line endings on Windows so checkout bytes
match this authenticated SHA256 input.
