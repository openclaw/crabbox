import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path

spec = importlib.util.spec_from_file_location("live_smoke", Path(__file__).with_name("live-smoke.py"))
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)


class VerificationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "screen.png").write_bytes(b"\x89PNG\r\n\x1a\n" + b"a" * 65536)
        with zipfile.ZipFile(self.root / "trace.zip", "w") as trace:
            trace.writestr("trace.trace", '{"type":"before"}\n')

    def record(self, mode="success"):
        code = 23 if mode == "failure" else 0
        (self.root / "result.json").write_text(json.dumps({"mode": mode, "exitCode": code}))
        files = {name: smoke.digest((self.root / name).read_bytes())
                 for name in ("screen.png", "trace.zip", "result.json")}
        return "ARTIFACT_PROOF " + json.dumps({"mode": mode, "exitCode": code, "files": files})

    def test_success_and_deliberate_failure(self):
        for mode in ("success", "failure"):
            with self.subTest(mode=mode):
                self.assertEqual(smoke.verify(self.record(mode), self.root, mode)["mode"], mode)

    def test_tampered_download_fails(self):
        record = self.record()
        (self.root / "screen.png").write_bytes(b"changed")
        with self.assertRaisesRegex(ValueError, "hash mismatch"):
            smoke.verify(record, self.root, "success")

    def test_duplicate_remote_record_fails(self):
        record = self.record()
        with self.assertRaisesRegex(ValueError, "exactly one"):
            smoke.verify(record + "\n" + record, self.root, "success")

    def test_wrong_case_fails(self):
        with self.assertRaisesRegex(ValueError, "identity"):
            smoke.verify(self.record("failure"), self.root, "success")

    def test_small_png_fails_even_with_matching_hash(self):
        (self.root / "screen.png").write_bytes(b"\x89PNG\r\n\x1a\n")
        with self.assertRaisesRegex(ValueError, "large binary"):
            smoke.verify(self.record(), self.root, "success")

    def test_non_trace_zip_fails(self):
        with zipfile.ZipFile(self.root / "trace.zip", "w") as trace:
            trace.writestr("unrelated.txt", "not a trace")
        with self.assertRaisesRegex(ValueError, "Playwright trace"):
            smoke.verify(self.record(), self.root, "success")

    def test_orchestration_reuses_and_stops_exact_lease(self):
        fake = self.root / "crabbox"
        fake.write_text('''#!/usr/bin/env python3
import hashlib,io,json,pathlib,sys,tarfile,zipfile
a=sys.argv[1:]; root=pathlib.Path.cwd(); lease="cbx_localtest"
if a[0]=="stop":
 assert a[-1]==lease
 (root/"stopped").write_text(lease)
 sys.exit(0)
reuse="--id" in a
if reuse: assert a[a.index("--id")+1]==lease and "--no-sync" in a
else:
 pathlib.Path(a[a.index("--lease-output")+1]).write_text(json.dumps({"provider":"cloudflare","leaseId":lease}))
mode="failure" if reuse else "success"; code=23 if reuse else 0
z=io.BytesIO()
with zipfile.ZipFile(z,"w") as f: f.writestr("test.trace","{}")
files={"screen.png":b"\\x89PNG\\r\\n\\x1a\\n"+b"x"*65536,"trace.zip":z.getvalue(),"result.json":json.dumps({"mode":mode,"exitCode":code}).encode()}
for i,v in enumerate(a):
 if v=="--download":
  remote,local=a[i+1].split("=",1);pathlib.Path(local).write_bytes(files[remote.split("/")[-1]])
archive=root/".crabbox"/"runs"/mode/"delegated-artifacts.tgz"; archive.parent.mkdir(parents=True)
with tarfile.open(archive,"w:gz") as f:
 for name,data in files.items():
  info=tarfile.TarInfo("reports/"+name);info.size=len(data);f.addfile(info,io.BytesIO(data))
print("ARTIFACT_PROOF "+json.dumps({"mode":mode,"exitCode":code,"files":{n:{"bytes":len(d),"sha256":hashlib.sha256(d).hexdigest()} for n,d in files.items()}}))
sys.exit(code)
''')
        fake.chmod(0o700)
        output = self.root / "output"
        result = subprocess.run([sys.executable, str(Path(smoke.__file__)), "--crabbox", str(fake),
                                 "--runner-url", "https://candidate.example.test", "--output", str(output),
                                 "--execute"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        receipt = json.loads((output / "receipt.json").read_text())
        self.assertEqual(receipt["failure"]["exitCode"], 23)
        self.assertEqual(receipt["stop"]["exitCode"], 0)
        self.assertEqual((output / "fixture" / "stopped").read_text(), "cbx_localtest")


if __name__ == "__main__":
    unittest.main()
