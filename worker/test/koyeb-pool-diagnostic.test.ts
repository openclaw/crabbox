import { execFile } from "node:child_process";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import { expect, it } from "vitest";

import {
  installPoolDiagnostic,
  projectPoolDiagnostic,
  readPoolDiagnostics,
} from "./fixtures/koyeb-pool-diagnostic";

const execute = promisify(execFile);
const observation = {
  operation: "pool_claim",
  stage: "home_integrity",
  reason: "home_changed",
  exception: "value_error",
  errno: "none",
};
const homeEntry = {
  schema: "crabbox-home-entry/v1",
  event: "runner_home_entry",
  operation: "pool_claim",
  reference: "bootstrap",
  field: "entry_added",
  pathClass: "home_top_level",
  count: "one",
  entryClass: "ice_authority_candidate",
  kind: "file",
  payload: "empty",
};

it("retains only fixed diagnostic vocabulary and discards extra data", () => {
  expect(
    projectPoolDiagnostic(
      JSON.stringify({
        ...observation,
        path: "/private/synthetic",
        content: "synthetic-secret",
        hash: "a".repeat(64),
        token: "credential",
      }),
    ),
  ).toEqual({ event: "runner_pool_diagnostic", ...observation });
});

it.each(Object.keys(observation))("rejects noncategorical %s without echoing it", (key) => {
  expect(
    projectPoolDiagnostic(JSON.stringify({ ...observation, [key]: "/private/synthetic-secret" })),
  ).toEqual({ event: "runner_pool_diagnostic_unavailable" });
});

it.each(["malformed synthetic-secret", "null", "[]", "{}"])(
  "omits invalid diagnostic data",
  (line) => {
    expect(projectPoolDiagnostic(line)).toEqual({ event: "runner_pool_diagnostic_unavailable" });
  },
);

it.each(["pool_check", "pool_claim"])(
  "retains only the strict %s HOME entry vocabulary",
  (operation) => {
    const entry = { ...homeEntry, operation };
    expect(projectPoolDiagnostic(JSON.stringify(entry))).toEqual(entry);
    expect(projectPoolDiagnostic(JSON.stringify({ ...entry, path: ".ICEauthority" }))).toEqual({
      event: "runner_pool_diagnostic_unavailable",
    });
    expect(projectPoolDiagnostic(JSON.stringify({ ...entry, kind: "directory" }))).toEqual({
      event: "runner_pool_diagnostic_unavailable",
    });
    expect(projectPoolDiagnostic(JSON.stringify({ ...entry, operation: "baseline" }))).toEqual({
      event: "runner_pool_diagnostic_unavailable",
    });
  },
);

it("uses a valid isolated Python startup loader without a private-executor request", async () => {
  const commands: string[][] = [];
  await installPoolDiagnostic(async (args) => {
    commands.push(args);
    return "";
  }, "synthetic-container");
  expect(commands.map((args) => args[0])).toEqual(["exec", "cp", "exec"]);
  const loader = commands[2]!.at(-1)!;
  const result = await execute("python3", [
    "-c",
    [
      "import sys,types",
      "sys.modules['crabbox_pool_diagnostic']=types.ModuleType('crabbox_pool_diagnostic')",
      "exec(sys.argv[1])",
      "assert '/tmp/crabbox-pool-diagnostic' in sys.path",
    ].join(";"),
    loader,
  ]);
  expect(result.stdout).toBe("");
  expect(result.stderr).toBe("");
});

it("bounds collection and projects records before logging", async () => {
  expect(
    await readPoolDiagnostics(async () => "secret\n".repeat(129), "synthetic-container"),
  ).toEqual([{ event: "runner_pool_diagnostic_unavailable" }]);
  expect(
    await readPoolDiagnostics(async () => JSON.stringify(observation), "synthetic-container"),
  ).toEqual([{ event: "runner_pool_diagnostic", ...observation }]);
});

it.each(
  ["pool_check", "pool_claim"].flatMap((operation) =>
    [
      ["Desktop", "desktop_candidate"],
      [".dbus", "dbus_candidate"],
      ["synthetic-private-entry", "other_top_level"],
    ].map(([name, entryClass]) => ({ operation, name, entryClass })),
  ),
)(
  "preserves the native $operation failure after adding $name",
  async ({ operation, name, entryClass }) => {
    const observer = fileURLToPath(new URL("./fixtures/koyeb-pool-diagnostic.py", import.meta.url));
    const helper = fileURLToPath(
      new URL("../../images/koyeb-sandbox-runner/project-state.py", import.meta.url),
    );
    const script = String.raw`
import importlib.util,json,os,pathlib,runpy,sys,tempfile
from unittest.mock import patch
observer_path,helper_path,operation,name,enabled=sys.argv[1:]
sys.path.insert(0,str(pathlib.Path(helper_path).parent))
def load(module_name,path):
    spec=importlib.util.spec_from_file_location(module_name,path)
    module=importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module
state=load("real_project_state",helper_path)
observer=load("pool_observer",observer_path)
observer.HELPER=helper_path
with tempfile.TemporaryDirectory(prefix="crabbox-home-entry-") as tmp:
    root=pathlib.Path(tmp)
    home=root/"home";work=root/"work";control=root/"control"
    for path in (home,work,control):path.mkdir()
    observer.OUTPUT=str(root/"events.jsonl")
    observer.emitted=0
    if enabled=="yes":
        with patch.object(sys,"argv",[helper_path]),patch.object(observer.os,"geteuid",return_value=0):
            observer.activate()
    state.pool_baseline(control,home)
    observer.finish_projection()
    if operation=="pool_claim":
        state.pool_clean(control,work,home)
        observer.finish_projection()
    (home/name).mkdir()
    (control/"lease-id").write_text("synthetic-lease")
    request_name="project-request-00000000-0000-0000-0000-000000000000.json"
    request=control/request_name
    request.write_text(json.dumps({"action":operation.replace("_","-"),"leaseID":"synthetic-lease","claim":"a"*64}))
    request_path="/var/lib/crabbox-koyeb/"+request_name
    paths={"/var/lib/crabbox-koyeb":control,"/workspace/crabbox":work,"/home/crabbox":home,request_path:request}
    real_path=type(root);real_open=os.open;real_unlink=os.unlink;real_fstat=os.fstat
    request_identity=request.stat()
    def redirect(path):return paths.get(os.fspath(path),path)
    def fixture_open(path,*args,**kwargs):
        return real_open(redirect(path),*args,**kwargs)
    def fixture_fstat(fd):
        info=real_fstat(fd)
        if (info.st_dev,info.st_ino)!=(request_identity.st_dev,request_identity.st_ino):return info
        values=list(info);values[4]=0
        return os.stat_result(values)
    try:
        # Redirect only native CLI fixture paths and the request's root ownership.
        # The unmodified helper supplies dispatch, HOME checks and terminal exit.
        with patch.object(pathlib,"Path",side_effect=lambda path,*rest:real_path(redirect(path),*rest)), \
             patch.object(os,"open",side_effect=fixture_open), \
             patch.object(os,"unlink",side_effect=lambda path,*args,**kwargs:real_unlink(redirect(path),*args,**kwargs)), \
             patch.object(os,"fstat",side_effect=fixture_fstat), \
             patch.dict(os.environ,{"CRABBOX_PROJECT_STATE_REQUEST_PATH":request_path}), \
             patch.object(sys,"argv",[helper_path]):
            runpy.run_path(helper_path,run_name="__main__")
    finally:
        sys.settrace(None)
        assert not (control/"pool-consumed.json").exists()
        events=real_path(observer.OUTPUT)
        if events.exists():print(events.read_text(),end="")
`;
    const invoke = (enabled: string) =>
      execute("python3", ["-c", script, observer, helper, operation, name!, enabled]).then(
        (result) => ({ code: 0, ...result }),
        (error: { code: number; stdout: string; stderr: string }) => error,
      );
    const plain = await invoke("no");
    const observed = await invoke("yes");
    expect(plain.code).toBe(2);
    expect(observed.code).toBe(plain.code);
    expect(plain.stdout).toBe("");
    expect(observed.stderr).toBe(plain.stderr);
    expect(observed.stderr).toBe('{"error": "checkpoint_project_state_failed"}\n');
    const records = observed.stdout
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line));
    expect(records).toContainEqual({
      operation,
      stage: "home_integrity",
      reason: "home_changed",
      exception: "value_error",
      errno: "none",
    });
    const entries = records.filter((record) => record.event === "runner_home_entry");
    expect(entries).toHaveLength(operation === "pool_check" ? 1 : 2);
    for (const entry of entries) {
      expect(entry).toEqual({
        ...homeEntry,
        operation,
        reference: entry.reference,
        entryClass,
        kind: "directory",
        payload: "not_applicable",
      });
      expect(projectPoolDiagnostic(JSON.stringify(entry))).toEqual(entry);
    }
    expect(entries.map((entry) => entry.reference)).toEqual(
      operation === "pool_check" ? ["bootstrap"] : ["bootstrap", "last_clean"],
    );
    for (const privateValue of [name!, "synthetic-lease", "a".repeat(64), "crabbox-home-entry-"]) {
      expect(observed.stdout + observed.stderr).not.toContain(privateValue);
    }
  },
);

it("preserves real pool check and claim results and terminal filesystem failures with observation", async () => {
  const observer = fileURLToPath(new URL("./fixtures/koyeb-pool-diagnostic.py", import.meta.url));
  const helper = fileURLToPath(
    new URL("../../images/koyeb-sandbox-runner/project-state.py", import.meta.url),
  );
  const script = String.raw`
import errno,fcntl,hashlib,importlib.util,json,os,pathlib,sys,tempfile,unittest
from unittest.mock import patch
observer_path, helper_path = sys.argv[1:]
sys.path.insert(0,str(pathlib.Path(helper_path).parent))
def load(name,path):
    spec=importlib.util.spec_from_file_location(name,path)
    module=importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module
state=load("real_project_state",helper_path)
observer=load("pool_observer",observer_path)
assert hashlib.sha256(pathlib.Path(helper_path).read_bytes()).hexdigest()==observer.EXPECTED_HELPER_SHA256
observer.HELPER=helper_path
class PoolObserverTests(unittest.TestCase):
    def exercise(self,kind,enabled):
        with tempfile.TemporaryDirectory(prefix="crabbox-observer-boundary-") as tmp:
            root=pathlib.Path(tmp)
            home=root/"home"; work=root/"work"; control=root/"control"
            for path in (home,work,control):path.mkdir()
            observer.OUTPUT=str(root/"events.jsonl")
            observer.operation="request";observer.emitted=0
            if enabled:
                with patch.object(sys,"argv",[helper_path]),patch.object(observer.os,"geteuid",return_value=0):
                    observer.activate()
            error=None; results=[];lock=None
            try:
                state.pool_baseline(control,home)
                if enabled:observer.finish_projection()
                try:
                    if kind=="home_check":
                        (home/".ICEauthority").write_bytes(b"")
                    results.append(state.pool_clean(control,work,home))
                    if enabled:observer.finish_projection()
                    if kind=="home":
                        (home/".ICEauthority").write_bytes(b"")
                    if kind=="lock":
                        lock=os.open(control/"pool.lock",os.O_CREAT|os.O_RDWR,0o600)
                        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
                    if kind=="sync":
                        with patch.object(state.os,"fsync",side_effect=OSError(errno.EIO,"synthetic-private-error","/private/synthetic")):
                            state.pool_clean(control,work,home,"a"*64)
                    else:
                        results.append(state.pool_clean(control,work,home,"a"*64))
                    if kind=="replay":
                        results.append(state.pool_clean(control,work,home,"a"*64))
                        state.pool_clean(control,work,home,"b"*64)
                except Exception as failure:
                    error=(type(failure).__name__,str(failure),getattr(failure,"errno",None))
            finally:
                if enabled and kind in ("home","home_check"):observer.finish_projection()
                sys.settrace(None)
                if lock is not None:os.close(lock)
            marker=control/"pool-consumed.json"
            marker_content=marker.read_bytes() if marker.exists() else None
            log=pathlib.Path(observer.OUTPUT).read_text() if pathlib.Path(observer.OUTPUT).exists() else ""
            records=[json.loads(line) for line in log.splitlines()]
            self.assertNotIn("synthetic-private",log)
            self.assertNotIn("/private/synthetic",log)
            self.assertNotIn("a"*64,log)
            self.assertNotIn(str(root),log)
            self.assertNotIn(".ICEauthority",log)
            return (results,error,marker_content),records
    def test_first_pool_check_home_failure_remains_exact(self):
        plain,_=self.exercise("home_check",False);observed,records=self.exercise("home_check",True)
        self.assertEqual(plain,observed)
        self.assertEqual(observed[0],[])
        self.assertEqual(observed[1][0:2],("ValueError","runner home changed after clean bootstrap"))
        self.assertIsNone(observed[2])
        self.assertTrue(any(r.get("operation")=="pool_check" and r.get("stage")=="home_integrity" and r.get("reason")=="home_changed" for r in records))
        self.assertEqual([r for r in records if r.get("event")=="runner_home_entry"],[
            {"schema":"crabbox-home-entry/v1","event":"runner_home_entry","operation":"pool_check",
             "reference":"bootstrap","field":"entry_added","pathClass":"home_top_level","count":"one",
             "entryClass":"ice_authority_candidate","kind":"file","payload":"empty"},
        ])
        self.assertEqual([r for r in records if r.get("event")=="runner_home_comparison"],[
            {"event":"runner_home_comparison","operation":"pool_check","reference":"bootstrap",
             "reason":"changed","pathClass":"home_top_level","field":"entry_added","count":"one"},
            {"event":"runner_home_comparison","operation":"pool_check","reference":"last_clean",
             "reason":"projection_missing","pathClass":"none","field":"none","count":"none"},
        ])
    def test_home_integrity_failure_remains_exact(self):
        plain,_=self.exercise("home",False);observed,records=self.exercise("home",True)
        self.assertEqual(plain,observed)
        self.assertEqual(observed[1][0:2],("ValueError","runner home changed after clean bootstrap"))
        self.assertIsNone(observed[2])
        self.assertTrue(any(r.get("stage")=="home_integrity" and r.get("reason")=="home_changed" for r in records))
        self.assertEqual([r for r in records if r.get("event")=="runner_home_entry"],[
            {"schema":"crabbox-home-entry/v1","event":"runner_home_entry","operation":"pool_claim",
             "reference":"bootstrap","field":"entry_added","pathClass":"home_top_level","count":"one",
             "entryClass":"ice_authority_candidate","kind":"file","payload":"empty"},
            {"schema":"crabbox-home-entry/v1","event":"runner_home_entry","operation":"pool_claim",
             "reference":"last_clean","field":"entry_added","pathClass":"home_top_level","count":"one",
             "entryClass":"ice_authority_candidate","kind":"file","payload":"empty"},
        ])
    def test_claim_replay_and_other_token_denial_remain_exact(self):
        plain,_=self.exercise("replay",False);observed,records=self.exercise("replay",True)
        self.assertEqual(plain,observed)
        self.assertEqual(observed[1][0:2],("ValueError","runner has already been consumed"))
        self.assertEqual([r["state"] for r in observed[0]],["clean","claimed","claimed"])
        self.assertTrue(any(r.get("stage")=="consumed_marker" and r.get("reason")=="already_consumed" for r in records))
    def test_lock_failure_remains_terminal(self):
        plain,_=self.exercise("lock",False);observed,records=self.exercise("lock",True)
        self.assertEqual(plain,observed)
        self.assertIsNotNone(observed[1])
        self.assertIsNone(observed[2])
        self.assertTrue(any(r.get("stage")=="lock_acquire" and r.get("exception")=="lock_blocked" for r in records))
    def test_marker_sync_failure_preserves_error_and_redacts_message(self):
        plain,_=self.exercise("sync",False);observed,records=self.exercise("sync",True)
        self.assertEqual(plain,observed)
        self.assertEqual(observed[1][2],errno.EIO)
        self.assertTrue(any(r.get("stage")=="claim_marker_sync" and r.get("errno")=="EIO" for r in records))
    def test_source_mismatch_disables_observer(self):
        with tempfile.TemporaryDirectory(prefix="crabbox-observer-mismatch-") as tmp:
            observer.OUTPUT=str(pathlib.Path(tmp)/"events.jsonl");observer.emitted=0
            with patch.object(sys,"argv",[helper_path]),patch.object(observer.os,"geteuid",return_value=0),patch.object(observer,"EXPECTED_HELPER_SHA256","0"*64):
                observer.activate()
            self.assertIsNone(sys.gettrace())
            records=[json.loads(line) for line in pathlib.Path(observer.OUTPUT).read_text().splitlines()]
            self.assertEqual(records[-1]["reason"],"source_mismatch")
unittest.main(argv=["pool-observer-boundaries"],verbosity=2)
`;
  const result = await execute("python3", ["-c", script, observer, helper]);
  expect(result.stdout).toBe("");
  expect(result.stderr).toContain("Ran 6 tests");
  expect(result.stderr).toContain("OK");
});
