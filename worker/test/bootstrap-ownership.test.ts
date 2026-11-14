import { spawnSync } from "node:child_process";
import {
  lchownSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

import { cloudInit } from "../src/bootstrap";
import { leaseConfig } from "../src/config";

// Exercise the emitted bootstrap commands, not a second implementation of the repair.
function ownershipScript(workRoot: string, cacheRoot: string): string {
  const generated = cloudInit(
    leaseConfig({ provider: "aws", sshUser: "root", workRoot, sshPublicKey: "fixture" }),
  );
  const start = generated.indexOf("      mkdir -p '");
  const end = generated.indexOf("      install -d /var/lib/crabbox", start);
  expect(start).toBeGreaterThan(0);
  expect(end).toBeGreaterThan(start);
  return generated
    .slice(start, end)
    .split("\n")
    .map((line) => line.slice(6))
    .join("\n")
    .replace(/'\/var\/cache\/crabbox'|\/var\/cache\/crabbox\/(?:pnpm|npm)/g, (path) => {
      const target =
        path === "'/var/cache/crabbox'" ? cacheRoot : path.replace("/var/cache/crabbox", cacheRoot);
      return "'" + target.replaceAll("'", "'\\''") + "'";
    });
}

function metadata(path: string) {
  const stat = lstatSync(path, { bigint: true });
  return { uid: stat.uid, gid: stat.gid, ctime: stat.ctimeNs, mode: stat.mode };
}

function entries(path: string): string[] {
  return [
    path,
    ...(lstatSync(path).isDirectory()
      ? readdirSync(path).flatMap((name) => entries(join(path, name)))
      : []),
  ];
}

function run(script: string) {
  const result = spawnSync("bash", ["--noprofile", "--norc", "-euo", "pipefail", "-c", script], {
    encoding: "utf8",
  });
  expect(result.stderr).toBe("");
  expect(result.status).toBe(0);
}

describe("cloud-init ownership repair", () => {
  it("emits a physical, descriptor-anchored numeric ownership repair", () => {
    const script = ownershipScript("/work/fixture's tree", "/tmp/cache");
    expect(script).toContain("python3 -I - 'root' '/work/fixture'\\''s tree'");
    expect(script).toContain("uid = pwd.getpwnam(sys.argv[1]).pw_uid");
    expect(script).toContain("gid = grp.getgrnam(sys.argv[1]).gr_gid");
    expect(script).toContain("if info.st_uid != uid or info.st_gid != gid:");
    expect(script).toContain(
      "os.chown(name, uid, gid, dir_fd=directory_fd, follow_symlinks=False)",
    );
    expect(script).toContain("os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent_fd");
    expect(script).toContain("PATH=/usr/sbin:/usr/bin:/sbin:/bin python3 -I");
    expect(script).not.toMatch(/^chown -R/m);
    const result = spawnSync("bash", ["-n"], { input: script, encoding: "utf8" });
    expect(result.stderr).toBe("");
    expect(result.status).toBe(0);
  });

  // Changing arbitrary numeric owners requires root on the actual GNU/Linux target.
  // Run these in a disposable Linux container; macOS chown has different semantics.
  it
    .skipIf(process.platform !== "linux" || process.getuid?.() !== 0)
    .each(["correct", "incorrect", "mixed"])(
    "preserves correct inodes and repairs %s ownership without following links",
    (scenario) => {
      const fixture = mkdtempSync(join(tmpdir(), "crabbox-ownership-"));
      try {
        const work = join(fixture, "work's tree");
        const cache = join(fixture, "cache's tree");
        const outside = join(fixture, "outside");
        for (const path of [work, join(work, "nested"), cache, outside]) mkdirSync(path);
        for (const path of [join(cache, "npm"), join(cache, "pnpm")]) mkdirSync(path);
        const files = ["correct", "uid only", "gid\nonly", "nested/-both"];
        for (const name of files) writeFileSync(join(work, name), "fixture\n");
        writeFileSync(join(cache, "npm", "cached"), "cache\n");
        writeFileSync(join(outside, "file"), "outside\n");
        symlinkSync(outside, join(work, "directory-link"));
        symlinkSync(join(outside, "file"), join(work, "file-link"));
        symlinkSync(join(outside, "missing"), join(work, "dangling-link"));
        symlinkSync(".", join(work, "cycle-link"));
        symlinkSync("correct", join(work, "internal-link"));
        const paths = [...entries(work), ...entries(cache)];
        if (scenario === "incorrect") {
          for (const path of paths) lchownSync(path, 12345, 23456);
        } else if (scenario === "mixed") {
          lchownSync(join(work, "uid only"), 12345, 0);
          lchownSync(join(work, "gid\nonly"), 0, 23456);
          lchownSync(join(work, "nested"), 12345, 23456);
          lchownSync(join(work, "nested/-both"), 12345, 23456);
          lchownSync(join(work, "dangling-link"), 12345, 23456);
          lchownSync(join(cache, "npm", "cached"), 0, 23456);
        }
        const before = new Map(paths.map((path) => [path, metadata(path)]));
        const external = new Map(entries(outside).map((path) => [path, metadata(path)]));
        run(ownershipScript(work, cache));
        for (const path of paths) {
          const after = metadata(path);
          expect({ path, uid: after.uid, gid: after.gid }).toEqual({ path, uid: 0n, gid: 0n });
        }
        const correct = paths.filter((path) => {
          const old = before.get(path)!;
          return old.uid === 0n && old.gid === 0n;
        });
        for (const path of correct)
          expect({ path, ...metadata(path) }).toEqual({ path, ...before.get(path)! });
        const incorrect = paths.filter((path) => !correct.includes(path));
        for (const path of incorrect)
          expect(metadata(path).ctime).toBeGreaterThan(before.get(path)!.ctime);
        for (const [path, old] of external)
          expect({ path, ...metadata(path) }).toEqual({ path, ...old });
        const repaired = new Map(paths.map((path) => [path, metadata(path)]));
        run(ownershipScript(work, cache));
        for (const [path, old] of repaired)
          expect({ path, ...metadata(path) }).toEqual({ path, ...old });
      } finally {
        rmSync(fixture, { recursive: true, force: true });
      }
    },
  );

  it.skipIf(process.platform !== "linux" || process.getuid?.() !== 0)(
    "matches recursive chown for symlink roots while leaving their targets intact",
    () => {
      const fixture = mkdtempSync(join(tmpdir(), "crabbox-ownership-roots-"));
      try {
        const target = join(fixture, "target");
        mkdirSync(target);
        mkdirSync(join(target, "npm"));
        mkdirSync(join(target, "pnpm"));
        writeFileSync(join(target, "child"), "outside\n");
        const roots = [join(fixture, "work"), join(fixture, "cache")];
        for (const path of roots) symlinkSync(target, path);
        for (const path of [...entries(target), ...roots]) lchownSync(path, 12345, 23456);
        const external = new Map(entries(target).map((path) => [path, metadata(path)]));
        run(ownershipScript(roots[0], roots[1]));
        for (const root of roots)
          expect([metadata(root).uid, metadata(root).gid]).toEqual([0n, 0n]);
        for (const [path, old] of external)
          expect({ path, ...metadata(path) }).toEqual({ path, ...old });
        // Reset only our symlinks and compare the original bootstrap's behavior.
        for (const path of roots) lchownSync(path, 12345, 23456);
        run(`chown -R root:root '${roots[0]}' '${roots[1]}'`);
        for (const root of roots)
          expect([metadata(root).uid, metadata(root).gid]).toEqual([0n, 0n]);
        for (const [path, old] of external)
          expect({ path, ...metadata(path) }).toEqual({ path, ...old });
      } finally {
        rmSync(fixture, { recursive: true, force: true });
      }
    },
  );

  it.skipIf(process.platform !== "linux" || process.getuid?.() !== 0).each(["correct", "mixed"])(
    "repairs deep %s trees under the service soft descriptor limit",
    (scenario) => {
      const fixture = mkdtempSync(join(tmpdir(), "crabbox-ownership-deep-"));
      try {
        const work = join(fixture, "work");
        const cache = join(fixture, "cache");
        for (const path of [work, cache, join(cache, "npm"), join(cache, "pnpm")]) mkdirSync(path);
        let leaf = work;
        for (let depth = 0; depth < 600; depth++) {
          leaf = join(leaf, "d");
          mkdirSync(leaf);
        }
        writeFileSync(join(leaf, "file"), "deep fixture\n");
        if (scenario === "mixed") lchownSync(join(leaf, "file"), 12345, 23456);
        const paths = [...entries(work), ...entries(cache)];
        const before = new Map(paths.map((path) => [path, metadata(path)]));
        run(`ulimit -Sn 1024\n${ownershipScript(work, cache)}`);
        for (const path of paths) {
          const after = metadata(path);
          expect([after.uid, after.gid]).toEqual([0n, 0n]);
        }
        const correct = paths.filter((path) => {
          const old = before.get(path)!;
          return old.uid === 0n && old.gid === 0n;
        });
        for (const path of correct) expect(metadata(path)).toEqual(before.get(path));
      } finally {
        rmSync(fixture, { recursive: true, force: true });
      }
    },
  );
});
