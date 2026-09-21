# WayVNC controller handoff

WayVNC keeps layout ownership until the owning RFB client is destroyed.
Coordinator input ownership, a local socket close, and a newly connected viewer
do not establish that destruction. This protocol addresses
[issue 2076](https://github.com/openclaw/crabbox/issues/2076).

## Identity

Managed Linux Wayland bridges use a private Python 3 relay over their existing
authenticated SSH access when the coordinator advertises retirement protocol
version 1. Each relay opens one WayVNC control socket, subscribes to events, and
keeps that connection for its entire lifetime. It never reconnects control.
Linux peer credentials, boot ID, PID, and process start time bind the server
lifetime across relays.

The relay binds its RFB connection to a randomly selected, dedicated IPv4
loopback source address and completes RFB initialization in shared mode. It
rejects an already occupied address and requires exactly one matching client
in the authoritative list after initialization. This identifies its own live
connection only while the kernel still reports its TCP connection established,
without inferring identity from shared SSH loopback addresses,
client counts, or before/after list differences. The relay subsequently
provides the same RFB 3.8 shared desktop to its viewer. Managed WayVNC uses
SSH-only authentication; other RFB authentication modes retain the ordinary
bridge and manual guidance.

This is an ownership protocol inside the trusted guest account, not isolation
from another process with access to that account or its WayVNC control socket.
The private per-relay retirement socket lives in a mode-0700 temporary directory
under the desktop user's runtime directory and is removed on orderly exit.

## Retirement

On takeover, the coordinator marks the successor's handoff pending. The portal
keeps remote resizing disabled while pending. The coordinator sends the old
bridge a fresh request ID, the successor's bound client ID, the server lifetime,
and the known clients of current bridge connections in that lifetime.

The old relay checks the authoritative client list on its original control
connection. An unknown client, missing successor, lifetime mismatch, invalid
binding, or socket error aborts automatic handoff. It targets only its own
bound client with `client-disconnect`. Command success alone is insufficient:
it waits until an authoritative list excludes that exact client, retaining the
matching `client-disconnected` event when present. The successor must still be
present and no unknown clients may appear during this check. WayVNC 0.9.1 clears
layout ownership before its destruction callback emits that event.

The remote control operation has a five-second deadline; the SSH operation and
coordinator wait are bounded at seven and eight seconds respectively. The
bridge sends its acknowledgement before closing its coordinator connection.
Only the original bridge socket and matching request ID can acknowledge it.
The coordinator rechecks the current controller and viewer generation before
publishing a verified result; overlapping takeovers and reconnects cannot reuse
an earlier acknowledgement. The portal then enables resizing and clears the
manual reminder. The subsequent resize remains an RFB request: retirement does
not claim the compositor accepted a particular resolution.

Any ambiguity falls back to closing the previous sizing viewer and reconnecting
the current controller. A failed or overlapping handoff breaks the ownership
chain: later takeovers in that viewer group also retain manual guidance, because
an earlier observer may still own the layout. Closing all portal viewers or
resetting the bridge starts a fresh group; an old remote client that remains
connected is then an unknown client and still blocks automatic verification.
A coordinator restart discards in-memory bindings and
also falls back until fresh bridges register. Direct/native viewers and servers
without the supported control surface retain their existing behavior.
