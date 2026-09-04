import fs from "node:fs";
import path from "node:path";

export function writeExecutable(file, body) {
  fs.writeFileSync(file, body, "utf8");
  fs.chmodSync(file, 0o755);
}

export function copySmokeRepo(dir, sourceScript) {
  const tempRoot = path.join(dir, "repo");
  const tempScripts = path.join(tempRoot, "scripts");
  const smokeScript = path.join(tempScripts, path.basename(sourceScript));
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(sourceScript, smokeScript);
  fs.chmodSync(smokeScript, 0o755);
  return { tempRoot, smokeScript };
}

export function writeGoStub(binDir, scriptBody) {
  writeExecutable(
    path.join(binDir, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
out=""
while [[ "$#" -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then
    out="$2"
    shift 2
    continue
  fi
  shift
done
mkdir -p "$(dirname "$out")"
cat >"$out" <<'SCRIPT'
${scriptBody}
SCRIPT
chmod +x "$out"
`,
  );
}

export const shellArgHelper = `
arg_after() {
  local want="$1"
  shift
  while [[ "$#" -gt 0 ]]; do
    if [[ "$1" == "$want" ]]; then
      printf '%s' "$2"
      return 0
    fi
    shift
  done
  return 1
}
`;
