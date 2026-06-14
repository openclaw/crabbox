"""Tests for the reference Slurm external-provider adapter.

These tests exercise the adapter without a real Slurm cluster by faking the
``sbatch``/``squeue``/``sacct``/``scancel``/``ssh-keygen`` subprocess calls and by
substituting the per-job endpoint publication that the batch runner would
normally perform inside an allocation.

Run with::

    python3 -m pytest examples/slurm-external-provider/ -q
"""

from __future__ import annotations

import importlib.util
import json
import types
from pathlib import Path
from typing import Any, Dict, List

import pytest

MODULE_PATH = Path(__file__).with_name("slurm-cbx.py")


def _load_module() -> types.ModuleType:
    spec = importlib.util.spec_from_file_location("slurm_cbx", MODULE_PATH)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


slurm_cbx = _load_module()


# A canonical Crabbox lease id and the broker-generated identity that the
# external-provider protocol sends in ``desired``.
LEASE_ID = "cbx_0123456789ab"
SLUG = "slurm-smoke"
NAME = "crabbox-slurm-smoke-0a1b2c3d"


def make_args(state_dir: Path, runner: Path) -> types.SimpleNamespace:
    return types.SimpleNamespace(
        state_dir=str(state_dir),
        runner_script=str(runner),
        poll_interval=0.0,
    )


@pytest.fixture()
def runner_script(tmp_path: Path) -> Path:
    runner = tmp_path / "runner.sh"
    runner.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
    runner.chmod(0o755)
    return runner


@pytest.fixture()
def adapter(tmp_path: Path, runner_script: Path) -> slurm_cbx.SlurmCrabboxAdapter:
    state_dir = tmp_path / "state"
    return slurm_cbx.SlurmCrabboxAdapter(make_args(state_dir, runner_script))


class FakeSlurm:
    """Records subprocess invocations and drives job-state transitions.

    The fake stands in for both the module-level ``run`` helper (used for
    sbatch/ssh-keygen, which must succeed or raise) and ``subprocess.run`` (used
    for squeue/sacct/scancel, which are tolerant of non-zero exits).
    """

    def __init__(self, monkeypatch: pytest.MonkeyPatch, module: types.ModuleType) -> None:
        self.calls: List[List[str]] = []
        self.job_id = "123456"
        self.squeue_states: List[str] = ["PENDING", "RUNNING"]
        self.sacct_state = "COMPLETED"
        self.scancel_returncode = 0
        self.cancelled: List[str] = []
        self._module = module
        monkeypatch.setattr(module, "run", self._run)
        monkeypatch.setattr(module.subprocess, "run", self._subprocess_run)
        monkeypatch.setattr(module.shutil, "which", lambda name: f"/usr/bin/{name}")

    # ``run`` raises on failure, matching the real helper.
    def _run(self, command: List[str], env: Dict[str, str] | None = None):
        self.calls.append(list(command))
        prog = command[0]
        if prog == "sbatch":
            return types.SimpleNamespace(stdout=f"{self.job_id};cluster\n", stderr="", returncode=0)
        if prog == "ssh-keygen":
            # Emulate ssh-keygen by writing private + public key files.
            out_index = command.index("-f") + 1
            key_path = Path(command[out_index])
            key_path.write_text("PRIVATE KEY\n", encoding="utf-8")
            Path(str(key_path) + ".pub").write_text("ssh-ed25519 AAAA crabbox\n", encoding="utf-8")
            return types.SimpleNamespace(stdout="", stderr="", returncode=0)
        raise AssertionError(f"unexpected run() command: {command}")

    def _subprocess_run(self, command: List[str], **_: Any):
        self.calls.append(list(command))
        prog = command[0]
        if prog == "squeue":
            state = self.squeue_states.pop(0) if self.squeue_states else ""
            return types.SimpleNamespace(stdout=(state + "\n") if state else "", stderr="", returncode=0)
        if prog == "sacct":
            return types.SimpleNamespace(stdout=f"{self.sacct_state}\n", stderr="", returncode=0)
        if prog == "scancel":
            self.cancelled.append(command[-1])
            return types.SimpleNamespace(stdout="", stderr="", returncode=self.scancel_returncode)
        raise AssertionError(f"unexpected subprocess.run command: {command}")

    def names(self) -> List[str]:
        return [call[0] for call in self.calls]


@pytest.fixture()
def fake_slurm(monkeypatch: pytest.MonkeyPatch) -> FakeSlurm:
    return FakeSlurm(monkeypatch, slurm_cbx)


def _publish_endpoint(adapter: slurm_cbx.SlurmCrabboxAdapter, lease_id: str) -> None:
    """Simulate the batch runner writing endpoint.json inside the allocation."""
    endpoint_path = adapter.job_dir(lease_id) / "endpoint.json"
    endpoint_path.parent.mkdir(parents=True, exist_ok=True)
    endpoint_path.write_text(
        json.dumps(
            {
                "host": "node123.cluster.example.edu",
                "port": "39022",
                "user": "alice",
                "readyCheck": "command -v bash",
            }
        ),
        encoding="utf-8",
    )


def acquire_request(**overrides: Any) -> Dict[str, Any]:
    request = {
        "operation": "acquire",
        "desired": {"leaseId": LEASE_ID, "slug": SLUG, "name": NAME},
        "config": {},
    }
    request.update(overrides)
    return request


def test_doctor_reports_ready(adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm) -> None:
    response = adapter.handle({"operation": "doctor", "config": {}})
    assert response["protocolVersion"] == slurm_cbx.PROTOCOL_VERSION
    assert "ready" in response["message"]
    # doctor must not submit a job.
    assert "sbatch" not in fake_slurm.names()


def test_doctor_missing_commands(adapter: slurm_cbx.SlurmCrabboxAdapter, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(slurm_cbx.shutil, "which", lambda name: None)
    with pytest.raises(slurm_cbx.AdapterError) as excinfo:
        adapter.handle({"operation": "doctor", "config": {}})
    assert "missing Slurm command" in str(excinfo.value)


def test_acquire_resolve_release_happy_path(
    adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm
) -> None:
    # The runner publishes the endpoint immediately for the test.
    _publish_endpoint(adapter, LEASE_ID)

    response = adapter.handle(acquire_request())
    lease = response["lease"]

    assert response["protocolVersion"] == slurm_cbx.PROTOCOL_VERSION
    assert lease["leaseId"] == LEASE_ID
    assert lease["slug"] == SLUG
    assert lease["name"] == NAME
    assert lease["cloudId"] == "slurm/job/123456;cluster"
    assert lease["status"] == "ready"
    ssh = lease["ssh"]
    assert ssh["host"] == "node123.cluster.example.edu"
    assert ssh["port"] == "39022"
    assert ssh["user"] == "alice"
    # The generated per-job private key path is returned, never key contents.
    assert ssh["key"].endswith("id_ed25519")
    assert "PRIVATE" not in ssh["key"]

    # Reserved routing labels must not leak into the lease labels.
    for reserved in ("slug", "name", "lease", "externalResourceName", "externalResourceNameFromEnv"):
        assert reserved not in lease["labels"]
    assert lease["labels"]["slurmJobId"] == "123456;cluster"
    assert "sbatch" in fake_slurm.names()

    # resolve returns the same lease without resubmitting.
    fake_slurm.squeue_states = ["RUNNING"]
    resolve = adapter.handle(
        {
            "operation": "resolve",
            "id": LEASE_ID,
            "expected": {"leaseId": LEASE_ID, "slug": SLUG},
            "config": {},
        }
    )
    assert resolve["lease"]["leaseId"] == LEASE_ID
    assert resolve["lease"]["ssh"]["host"] == "node123.cluster.example.edu"

    # release cancels the persisted job and removes the job directory.
    release = adapter.handle(
        {
            "operation": "release",
            "id": LEASE_ID,
            "expected": {"leaseId": LEASE_ID, "slug": SLUG},
            "config": {},
        }
    )
    assert release["message"] == "released Slurm lease"
    assert fake_slurm.cancelled == ["123456"]
    assert not adapter.job_dir(LEASE_ID).exists()


def test_acquire_is_idempotent(adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm) -> None:
    _publish_endpoint(adapter, LEASE_ID)
    first = adapter.handle(acquire_request())
    submissions = fake_slurm.names().count("sbatch")
    assert submissions == 1

    fake_slurm.squeue_states = ["RUNNING"]
    second = adapter.handle(acquire_request())
    # A second acquire with the same lease id must not submit another job.
    assert fake_slurm.names().count("sbatch") == 1
    assert second["lease"]["cloudId"] == first["lease"]["cloudId"]


def test_lease_id_parsing_handles_parsable_cluster_suffix(
    adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm
) -> None:
    fake_slurm.job_id = "987654"
    _publish_endpoint(adapter, LEASE_ID)
    response = adapter.handle(acquire_request())
    state = adapter.load_state(LEASE_ID)
    assert state is not None
    # Numeric job id used for squeue/scancel; raw id retained for display.
    assert state["jobId"] == "987654"
    assert state["rawJobId"] == "987654;cluster"
    assert response["lease"]["labels"]["slurmJobId"] == "987654;cluster"


def test_acquire_cancels_when_endpoint_never_appears(
    adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm
) -> None:
    # No endpoint is published, and the scheduler reports a terminal state.
    fake_slurm.squeue_states = ["PENDING", "FAILED"]
    with pytest.raises(slurm_cbx.AdapterError) as excinfo:
        adapter.handle(acquire_request(config={"acquireTimeoutSeconds": 60}))
    assert "ended before publishing SSH endpoint" in str(excinfo.value)
    # Rollback cancelled the job and removed the state directory.
    assert fake_slurm.cancelled == ["123456"]
    assert not adapter.job_dir(LEASE_ID).exists()


def test_acquire_keeps_job_on_failure_when_keep_set(
    adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm
) -> None:
    fake_slurm.squeue_states = ["FAILED"]
    with pytest.raises(slurm_cbx.AdapterError):
        adapter.handle(acquire_request(keep=True, config={"acquireTimeoutSeconds": 60}))
    # keep=True must not cancel or delete the state on failure.
    assert fake_slurm.cancelled == []
    assert adapter.job_dir(LEASE_ID).exists()


def test_list_filters_terminal_and_omits_reserved_labels(
    adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm
) -> None:
    _publish_endpoint(adapter, LEASE_ID)
    adapter.handle(acquire_request())

    # Active job: squeue reports RUNNING -> stays in the default list.
    fake_slurm.squeue_states = ["RUNNING"]
    leases = adapter.handle({"operation": "list", "config": {}})["leases"]
    assert len(leases) == 1
    lease = leases[0]
    assert lease["leaseId"] == LEASE_ID
    for reserved in ("slug", "name", "lease", "externalResourceName", "externalResourceNameFromEnv"):
        assert reserved not in lease["labels"]

    # Terminal job: squeue empty, sacct COMPLETED -> filtered unless all=True.
    fake_slurm.squeue_states = []
    fake_slurm.sacct_state = "COMPLETED"
    default_list = adapter.handle({"operation": "list", "config": {}})["leases"]
    assert default_list == []

    fake_slurm.squeue_states = []
    fake_slurm.sacct_state = "COMPLETED"
    all_list = adapter.handle({"operation": "list", "all": True, "config": {}})["leases"]
    assert len(all_list) == 1


def test_cleanup_removes_terminal_state(
    adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm
) -> None:
    _publish_endpoint(adapter, LEASE_ID)
    adapter.handle(acquire_request())

    fake_slurm.squeue_states = []
    fake_slurm.sacct_state = "COMPLETED"
    dry = adapter.handle({"operation": "cleanup", "dryRun": True, "config": {}})
    assert "would remove 1" in dry["message"]
    assert adapter.job_dir(LEASE_ID).exists()

    fake_slurm.squeue_states = []
    fake_slurm.sacct_state = "COMPLETED"
    wet = adapter.handle({"operation": "cleanup", "config": {}})
    assert "removed 1" in wet["message"]
    assert not adapter.job_dir(LEASE_ID).exists()


def test_list_skips_corrupt_state_file(
    adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm
) -> None:
    _publish_endpoint(adapter, LEASE_ID)
    adapter.handle(acquire_request())

    # A second job dir with a truncated state.json must not break list/cleanup.
    bad_dir = adapter.state_dir / "jobs" / "cbx_badbadbadba"
    bad_dir.mkdir(parents=True, exist_ok=True)
    (bad_dir / "state.json").write_text("{not json", encoding="utf-8")

    fake_slurm.squeue_states = ["RUNNING"]
    leases = adapter.handle({"operation": "list", "config": {}})["leases"]
    assert [lease["leaseId"] for lease in leases] == [LEASE_ID]


def test_release_unknown_lease_is_noop(
    adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm
) -> None:
    response = adapter.handle({"operation": "release", "id": "cbx_ffffffffffff", "config": {}})
    assert response["message"] == "released Slurm lease"
    assert fake_slurm.cancelled == []


def test_validate_expected_mismatch_rejected(
    adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm
) -> None:
    _publish_endpoint(adapter, LEASE_ID)
    adapter.handle(acquire_request())
    with pytest.raises(slurm_cbx.AdapterError) as excinfo:
        adapter.handle(
            {
                "operation": "resolve",
                "id": LEASE_ID,
                "expected": {"leaseId": LEASE_ID, "slug": "different-slug"},
                "config": {},
            }
        )
    assert "does not match" in str(excinfo.value)


def test_proxy_through_login(adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm) -> None:
    _publish_endpoint(adapter, LEASE_ID)
    config = {"sshMode": "proxy-through-login", "loginHost": "login.cluster.example.edu"}
    response = adapter.handle(acquire_request(config=config))
    ssh = response["lease"]["ssh"]
    assert ssh["proxyCommand"] == "ssh -W %h:%p login.cluster.example.edu"
    assert ssh["sshConfigProxy"] is True


def test_proxy_through_login_requires_login_host(
    adapter: slurm_cbx.SlurmCrabboxAdapter, fake_slurm: FakeSlurm
) -> None:
    _publish_endpoint(adapter, LEASE_ID)
    with pytest.raises(slurm_cbx.AdapterError) as excinfo:
        adapter.handle(acquire_request(config={"sshMode": "proxy-through-login"}))
    assert "requires loginHost" in str(excinfo.value)


def test_sbatch_command_builds_resource_flags(
    adapter: slurm_cbx.SlurmCrabboxAdapter, runner_script: Path
) -> None:
    config = {
        "account": "lab",
        "partition": "batch",
        "cpus": 16,
        "mem": "64G",
        "timeLimit": "02:00:00",
        "gres": "gpu:1",
        "extraSbatchArgs": ["--exclusive"],
    }
    job_dir = adapter.job_dir(LEASE_ID)
    command = adapter.sbatch_command(config, NAME, job_dir, runner_script)
    assert command[0] == "sbatch"
    assert "--parsable" in command
    assert command[command.index("--account") + 1] == "lab"
    assert command[command.index("--cpus-per-task") + 1] == "16"
    assert command[command.index("--gres") + 1] == "gpu:1"
    assert "--exclusive" in command
    assert command[-1] == str(runner_script)


def test_sbatch_rejects_non_string_extra_args(
    adapter: slurm_cbx.SlurmCrabboxAdapter, runner_script: Path
) -> None:
    job_dir = adapter.job_dir(LEASE_ID)
    with pytest.raises(slurm_cbx.AdapterError):
        adapter.sbatch_command({"extraSbatchArgs": [123]}, NAME, job_dir, runner_script)


def test_query_job_state_parses_sacct_qualifier(
    adapter: slurm_cbx.SlurmCrabboxAdapter, monkeypatch: pytest.MonkeyPatch
) -> None:
    # squeue empty, sacct reports "CANCELLED by 1000" -> first token only.
    def fake_run(command, **_):
        if command[0] == "squeue":
            return types.SimpleNamespace(stdout="\n", stderr="", returncode=0)
        return types.SimpleNamespace(stdout="CANCELLED by 1000\n\n", stderr="", returncode=0)

    monkeypatch.setattr(slurm_cbx.subprocess, "run", fake_run)
    monkeypatch.setattr(slurm_cbx.shutil, "which", lambda name: f"/usr/bin/{name}")
    assert adapter.query_job_state("123456") == "CANCELLED"


def test_public_config_redacts_secrets() -> None:
    redacted = slurm_cbx.public_config({"account": "lab", "apiToken": "shh", "myPassword": "pw"})
    assert redacted["account"] == "lab"
    assert redacted["apiToken"] == "<redacted>"
    assert redacted["myPassword"] == "<redacted>"


def test_main_doctor_via_stdin(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, runner_script: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    import io
    import sys

    monkeypatch.setattr(slurm_cbx.shutil, "which", lambda name: f"/usr/bin/{name}")
    state_dir = tmp_path / "state"
    argv = [
        "slurm-cbx.py",
        "--state-dir",
        str(state_dir),
        "--runner-script",
        str(runner_script),
    ]
    monkeypatch.setattr(sys, "argv", argv)
    monkeypatch.setattr(sys, "stdin", io.StringIO(json.dumps({"operation": "doctor", "config": {}})))
    exit_code = slurm_cbx.main()
    assert exit_code == 0
    out = capsys.readouterr().out
    payload = json.loads(out)
    assert payload["protocolVersion"] == slurm_cbx.PROTOCOL_VERSION
    assert "ready" in payload["message"]
