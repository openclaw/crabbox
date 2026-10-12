import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

// Inspect multiline calls and resolve local aliases/key builders, including
// imported aliases. This deliberately checks every source file, not a list of
// known writers, so a new module cannot silently bypass the funnel.
function bypasses(source: string, fileName = "fixture.ts"): string[] {
  const definitions = new Map<string, string[]>();
  const define = (name: string, value: string) =>
    definitions.set(name, [...(definitions.get(name) ?? []), value]);
  for (const match of source.matchAll(/(?:const|let)\s+(\w+)\s*=\s*([^;\n]+)/g))
    define(match[1]!, match[2]!);
  for (const match of source.matchAll(/function\s+(\w+)\s*\([^)]*\)[^{]*\{([^}]*?)\}/g))
    define(match[1]!, match[2]!);
  for (const match of source.matchAll(/\bleaseKey\s+as\s+(\w+)/g)) define(match[1]!, "leaseKey");
  const isLeaseKey = (expression: string, seen = new Set<string>()): boolean => {
    const value = expression.trim();
    if (/^[\x60"']lease:|^leaseKey(?:\s*\(|$)/.test(value)) return true;
    if (/^[\x60"']/.test(value)) return false;
    const returned = /\breturn\s+([^;]+)/.exec(value);
    if (returned) return isLeaseKey(returned[1]!, seen);
    const head = /^([\w$]+)(?:\s*\(|\s*$)/.exec(value)?.[1];
    if (!head || seen.has(head)) return false;
    seen.add(head);
    return (definitions.get(head) ?? []).some((definition) => isLeaseKey(definition, seen));
  };
  const violations: string[] = [];
  for (const match of source.matchAll(/\.(?:put|delete)\s*(?:<([^>]+)>)?\s*\(/g)) {
    const start = match.index + match[0].length;
    let depth = 0;
    let quote = "";
    let end = start;
    for (; end < source.length; end++) {
      const character = source[end]!;
      if (quote) {
        if (character === "\\") end++;
        else if (character === quote) quote = "";
      } else if (["'", '"', "`"].includes(character)) quote = character;
      else if ("([{<".includes(character)) depth++;
      else if ((character === "," || character === ")") && depth === 0) break;
      else if (")]}>".includes(character)) depth--;
    }
    if (isLeaseKey(source.slice(start, end)) || match[1]?.trim() === "LeaseRecord") {
      violations.push(`${fileName}:${source.slice(0, match.index).split("\n").length}`);
    }
  }
  return violations;
}

async function sourceFiles(directory: string): Promise<string[]> {
  const entries = await readdir(directory, { withFileTypes: true });
  return (
    await Promise.all(
      entries.map(async (entry) => {
        const path = join(directory, entry.name);
        return entry.isDirectory() ? sourceFiles(path) : entry.name.endsWith(".ts") ? [path] : [];
      }),
    )
  ).flat();
}

describe("lease record write funnel invariant", () => {
  it("recognizes raw, generic, multiline, aliased and deleted lease writes", () => {
    for (const source of [
      "await storage.put(`lease:${id}`, lease)",
      "await transaction.put<LeaseRecord>(\n leaseKey(id), lease)",
      "const key = `lease:${id}`; await storage.delete(key)",
      "function recordKey(id) { return `lease:${id}`; } storage.put(recordKey(id), lease)",
      'import { leaseKey as key } from "./store"; transaction.delete(key(id))',
    ])
      expect(bypasses(source)).toHaveLength(1);
    expect(bypasses("storage.put(`lease-maintenance:${id}`, 1)")).toEqual([]);
    expect(bypasses("storage.put(`lease-current:${id}`, 1)")).toEqual([]);
    expect(bypasses('storage.put("workspace-lease:one", true)')).toEqual([]);
  });

  it("allows lease-record storage writes only in the shared helper", async () => {
    const root = new URL("../src/", import.meta.url).pathname;
    const files = [
      ...(await sourceFiles(root)),
      ...(await sourceFiles(new URL("../node/", import.meta.url).pathname)),
    ];
    const violations = (
      await Promise.all(
        files
          .filter((file) => !file.endsWith("/lease-record-store.ts"))
          .map(async (file) => bypasses(await readFile(file, "utf8"), file)),
      )
    ).flat();
    expect(violations).toEqual([]);
  });
});
