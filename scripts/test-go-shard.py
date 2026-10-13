#!/usr/bin/env python3
"""Run one exhaustive, disjoint shard of the CLI's top-level Go tests.

A test's shard depends only on its own name and SLOW_TESTS, never on which
other tests exist:

- Tests in SLOW_TESTS are placed heaviest first, each on the shard with the
  least SLOW_TESTS weight so far (lowest index on ties).
- Every other test runs on shard sha256(name) mod count.

Adding, removing, or renaming an unlisted test therefore moves no other test;
only edits to SLOW_TESTS move listed tests.
"""

import argparse
import hashlib
import heapq
import json
import re
import subprocess
import sys

# Top-level elapsed seconds from `go test -race -count=1 -json ./internal/cli`;
# only relative weights matter. List every test at 15 seconds or more: hashing
# tests that large leaves shard totals uneven. Discovery fails while a listed
# test is missing, so renaming one must update this list.
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


def coverage_test(command):
    """Stream Go's diagnostics and retain only ten top-level elapsed times."""
    slowest = []
    invalid = False
    with subprocess.Popen(command + ["-json"], stdout=subprocess.PIPE, text=True) as child:
        for line in child.stdout:
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                print(line, end="", file=sys.stderr)
                invalid = True
                continue
            print(event.get("Output", ""), end="", flush=True)
            name = event.get("Test", "")
            if event.get("Action") in ("pass", "fail") and name and "/" not in name:
                heapq.heappush(slowest, (event.get("Elapsed", 0), name))
                if len(slowest) > 10:
                    heapq.heappop(slowest)
        code = child.wait()
    print("Slowest top-level tests (elapsed seconds; nested durations are not summed):")
    for elapsed, name in sorted(slowest, reverse=True):
        print(f"  {elapsed:.3f} {name}")
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
        return coverage_test(command + ["-coverprofile=" + args.coverprofile])
    return subprocess.run(command).returncode


if __name__ == "__main__":
    sys.exit(main())
