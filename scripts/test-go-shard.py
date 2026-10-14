#!/usr/bin/env python3
"""Run one exhaustive, disjoint shard of the CLI's top-level Go tests.

A test's shard depends only on its own name and SLOW_TESTS, never on which
other tests exist:

- Tests in SLOW_TESTS are placed heaviest first, each on the shard with the
  least SLOW_TESTS weight so far (lowest index on ties).
- Every other test runs on shard sha256(name) mod count.

Adding, removing, or renaming an unlisted test therefore moves no other test;
only edits to SLOW_TESTS move listed tests.

Both lanes stream `go test -json` but print only what plain `go test` would:
package output and the output of failed or unfinished tests. Each shard ends
with the elapsed seconds of its listed tests and of unlisted tests at
SLOW_SECONDS or more; the race shards' summaries calibrate SLOW_TESTS.
"""

import argparse
import hashlib
import json
import re
import subprocess
import sys

# Top-level elapsed seconds from `go test -race -count=1 -json ./internal/cli`;
# only relative weights matter. List every test at 15 seconds or more: hashing
# tests that large leaves shard totals uneven. Discovery fails while a listed
# test is missing, so renaming one must update this list.
SLOW_SECONDS = 0
SLOW_TESTS = {
    "TestRunGitOverlaySuccessFallbackAndLateLocalEdit": 201,
    "TestRunDefaultSyncWithoutBash": 71,
    "TestCoordinatorStopAggregateGuestCleanupPreservesReleaseBudget": 64,
    "TestFunctionalPOSIXPreflightOwner": 59,
    "TestRunActionsWorkspaceRealFinalEffects": 54,
    "TestCheckpointCaptureBuiltBinaryContract": 42,
    "TestRunArtifactChangeWithFailureDownloadsE2E": 42,
    "TestRunArtifactChangeE2E": 37,
    "TestHybridSSHScriptRun": 32,
    "TestWaitForSSHReadyRealSSHTimeoutDiagnostic": 32,
    "TestRunScriptFailureBundleScopesRetainedUploads": 32,
    "TestVNCPasswordSSHProductionDeadline": 31,
    "TestHeartbeatCoordinatorWaitsForAccessRefresh": 31,
    "TestWebVNCWebSocketHeaderDeadlineNetwork": 31,
    "TestRunFunctionalPreflightContinuation": 30,
    "TestRunDetachedGitSeedPreservesManifestAuthority": 26,
    "TestRsyncStallTimeoutBehavior": 24,
    "TestRunDirectorySourceActionsWorkspaceRejected": 22,
    "TestRunCoordinatorCleanupOutcomes": 22,
    "TestStopCoordinatorStalledLookup": 21,
    "TestGitHubActionsRunnerSeedsOnlyOwnedDefaultToolCache": 21,
    "TestEgressOpenRemoteTimeoutLeaksPendingAndNeverClosesHostConn": 20,
    "TestWarmupCreationRejectedTiming": 19,
    "TestRemoteGitCoherenceRollsBackFailuresAndRetries": 19,
    "TestFixedAcquisitionSerializesCLIReplayAcrossProcesses": 19,
    "TestRunCommandLeaseCleanupQuiescesWorkspaceOwner": 19,
    "TestCommandSelectorPrecedence": 19,
    "TestRunLeaseRecoveryGuidance": 16,
    "TestCoordinatorStopReleasesAfterUnreachableGuestCleanup": 16,
    "TestRunAdmissionFailureRetainsEffectiveAuthenticationAndRedactsWarning": 16,
    "TestExecuteLocalActionsHydrationUsesCallerPlainManifestMode": 15,
}


def go_test(command):
    """Hold each top-level test's output until it ends; print it only on failure.

    A timeout panic can be attributed to a test that already passed, and the
    hung test never ends, so output still held at exit is printed too.
    """
    held, timings, invalid = {}, [], False
    with subprocess.Popen(command + ["-json"], stdout=subprocess.PIPE, text=True) as child:
        for line in child.stdout:
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                print(line, end="", file=sys.stderr)
                invalid = True
                continue
            name, action = event.get("Test", ""), event.get("Action")
            top = name.split("/")[0]
            if not top:
                print(event.get("Output", ""), end="", flush=True)
                continue
            held.setdefault(top, []).append(event.get("Output", ""))
            if name != top or action not in ("pass", "fail", "skip"):
                continue
            output = "".join(held.pop(top))
            if action == "fail":
                print(output, end="", flush=True)
            elapsed = event.get("Elapsed", 0)
            if action != "skip" and (name in SLOW_TESTS or elapsed >= SLOW_SECONDS):
                timings.append((elapsed, name))
        code = child.wait()
    for output in held.values():
        print("".join(output), end="")
    print(f"Top-level seconds for SLOW_TESTS and unlisted tests >= {SLOW_SECONDS} s (subtests not summed):")
    for elapsed, name in sorted(timings, reverse=True):
        print(f"  {elapsed:.3f} {name}" + ("" if name in SLOW_TESTS else " (unlisted)"))
    return code or int(invalid)


def select_tests(listing, shard, count):
    if count < 1 or not 0 <= shard < count:
        raise ValueError("shard must be in [0, count)")
    # Include fuzz seed corpora and examples, just as an ordinary go test does.
    names = sorted(set(re.findall(r"^(?:Test|Example|Fuzz)\w*$", listing, re.M)))
    if len(names) < count:
        raise ValueError("test discovery returned fewer tests than shards")
    missing = sorted(SLOW_TESTS.keys() - set(names))
    if missing:
        raise ValueError("SLOW_TESTS lists undiscovered tests: " + ", ".join(missing))
    owners, loads = {}, [0] * count
    for name in sorted(SLOW_TESTS, key=lambda name: (-SLOW_TESTS[name], name)):
        owners[name] = loads.index(min(loads))
        loads[owners[name]] += SLOW_TESTS[name]
    return [name for name in names if owners.get(name, hashed_shard(name, count)) == shard]


def hashed_shard(name, count):
    return int.from_bytes(hashlib.sha256(name.encode()).digest()[:8], "big") % count


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("shard", type=int)
    parser.add_argument("count", type=int)
    parser.add_argument("--coverprofile", help="opt into atomic coverage instead of the race lane")
    args = parser.parse_args()
    if args.count < 1 or not 0 <= args.shard < args.count:
        parser.error("shard must be in [0, count)")
    package = "./internal/cli"
    common = ["go", "test", "-race", "-count=1", "-timeout=10m", package]
    if args.coverprofile:
        common = ["go", "test", "-count=1", "-timeout=20m", "-covermode=atomic", package]
    listing = subprocess.run(common + ["-list", "."], text=True, capture_output=True)
    if listing.returncode:
        sys.stdout.write(listing.stdout)
        sys.stderr.write(listing.stderr)
        return listing.returncode
    names = select_tests(listing.stdout, args.shard, args.count)
    print(f"CLI shard {args.shard + 1}/{args.count}: {len(names)} tests", flush=True)
    command = common + ["-run", "^(" + "|".join(names) + ")$"]
    if args.coverprofile:
        command.append("-coverprofile=" + args.coverprofile)
    return go_test(command)


if __name__ == "__main__":
    sys.exit(main())
