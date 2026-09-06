import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import path from "node:path";
import test from "node:test";

const slug = "public-smoke-slug";
const redactionProbe = "public-test-token";
const matcher = path.join(import.meta.dirname, "lib", "live-smoke-json-match.py");

const adapters = [
  {
    name: "lambda",
    profile: "lambda",
    operations: ["contains", "empty"],
    interpreter: "python3",
  },
  {
    name: "nebius",
    profile: "nebius",
    operations: ["contains", "absent"],
    interpreter: "python3",
  },
  { name: "nvidia-brev", profile: "standard", operations: ["contains"], interpreter: "python3" },
  {
    name: "github-codespaces",
    profile: "standard",
    operations: ["contains", "absent", "probe"],
    interpreter: "configured-python",
  },
  {
    name: "runpod",
    profile: "runpod",
    operations: ["contains", "empty"],
    interpreter: "python3",
  },
  { name: "vast", profile: "vast", operations: ["contains", "empty"], interpreter: "python3" },
  { name: "docker-sandbox", profile: "standard", operations: ["contains"], interpreter: "python3" },
];

const cases = [
  { name: "empty-list", raw: "[]" },
  { name: "direct-slug", raw: JSON.stringify([{ slug }]) },
  { name: "direct-name", raw: JSON.stringify([{ name: slug }]) },
  { name: "direct-id", raw: JSON.stringify([{ id: slug }]) },
  { name: "direct-lease-id", raw: JSON.stringify([{ leaseId: slug }]) },
  { name: "labels-slug", raw: JSON.stringify([{ labels: { slug } }]) },
  { name: "tags-slug", raw: JSON.stringify([{ tags: { slug } }]) },
  { name: "labels-dotted-alias", raw: JSON.stringify([{ labels: { "crabbox.slug": slug } }]) },
  {
    name: "empty-labels-fall-through-to-tags",
    raw: JSON.stringify([{ labels: {}, tags: { "crabbox.slug": slug } }]),
  },
  { name: "labels-underscore-alias", raw: JSON.stringify([{ labels: { crabbox_slug: slug } }]) },
  { name: "vast-delimited-label", raw: JSON.stringify([{ label: `prefix|${slug}|suffix` }]) },
  { name: "nested-labels-slug", raw: JSON.stringify({ outer: [{ inner: { labels: { slug } } }] }) },
  { name: "partial-and-unrelated-values", raw: JSON.stringify([{ slug: `${slug}-other`, note: slug }]) },
  {
    name: "truthy-labels-mask-dotted-tag-alias",
    raw: JSON.stringify([{ labels: { other: true }, tags: { "crabbox.slug": slug } }]),
  },
  { name: "vast-list-label-stringification", raw: JSON.stringify([{ label: [`|${slug}|`] }]) },
  { name: "malformed-json", raw: "{" },
];

const baselineProgram = String.raw`
import json
import sys

adapter, operation, slug = sys.argv[1:4]
profile = {
    "lambda": "lambda",
    "nebius": "nebius",
    "nvidia-brev": "standard",
    "github-codespaces": "standard",
    "runpod": "runpod",
    "vast": "vast",
    "docker-sandbox": "standard",
}[adapter]

def has_slug(value):
    if isinstance(value, dict):
        if profile in ("lambda", "vast"):
            labels = value.get("labels") or value.get("tags")
        else:
            labels = value.get("labels")
        if isinstance(labels, dict):
            if labels.get("slug") == slug:
                return True
            if profile in ("lambda", "vast") and labels.get("crabbox.slug") == slug:
                return True
            if profile == "nebius" and labels.get("crabbox_slug") == slug:
                return True
        if profile != "runpod":
            if any(value.get(key) == slug for key in ("slug", "name", "id", "leaseId")):
                return True
        if profile == "vast" and f"|{slug}|" in str(value.get("label", "")):
            return True
        return any(has_slug(child) for child in value.values())
    if isinstance(value, list):
        return any(has_slug(child) for child in value)
    return False

try:
    payload = json.load(sys.stdin)
except Exception as exc:
    print(f"invalid JSON: {exc}", file=sys.stderr)
    sys.exit(2 if operation == "probe" else 1)

matched = has_slug(payload)
if operation == "probe":
    sys.exit(0 if matched else 1)
if operation == "empty":
    if payload != []:
        provider = {"lambda": "Lambda", "runpod": "RunPod", "vast": "Vast"}[adapter]
        print(f"{provider} Crabbox inventory is not empty", file=sys.stderr)
        sys.exit(1)
    sys.exit(0)
if operation == "contains" and not matched:
    print(f"list JSON did not include slug {slug}", file=sys.stderr)
    sys.exit(1)
if operation == "absent" and matched:
    verb = "included" if adapter == "github-codespaces" else "includes"
    print(f"list JSON still {verb} slug {slug}", file=sys.stderr)
    sys.exit(1)
`;

function runBaseline(adapter, operation, raw) {
  const result = spawnSync("python3", ["-c", baselineProgram, adapter.name, operation, slug], {
    input: `${raw}\n`,
    encoding: "utf8",
    env: {
      PATH: "/bin:/usr/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin",
    },
  });
  return {
    status: result.status,
    stdout: result.stdout,
    stderr: result.stderr,
    interpreter: adapter.interpreter,
    redacted: !result.stdout.includes(redactionProbe) && !result.stderr.includes(redactionProbe),
  };
}

function failureMessage(adapter, operation) {
  if (operation === "contains") {
    return `list JSON did not include slug ${slug}`;
  }
  if (operation === "absent") {
    const verb = adapter.name === "github-codespaces" ? "included" : "includes";
    return `list JSON still ${verb} slug ${slug}`;
  }
  if (operation === "empty") {
    const provider = { lambda: "Lambda", runpod: "RunPod", vast: "Vast" }[adapter.name];
    return `${provider} Crabbox inventory is not empty`;
  }
  return "";
}

function runMatcher(profile, operation, raw, failure = "") {
  return spawnSync("python3", [matcher, profile, operation], {
    input: `${raw}\n`,
    encoding: "utf8",
    env: {
      CRABBOX_SMOKE_FAILURE: failure,
      CRABBOX_SMOKE_SLUG: slug,
      PATH: "/bin:/usr/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin",
    },
  });
}

function runCandidate(adapter, operation, raw) {
  const matcherResult = runMatcher(
    adapter.profile,
    operation,
    raw,
    failureMessage(adapter, operation),
  );
  return {
    status: matcherResult.status,
    stdout: matcherResult.stdout,
    stderr: matcherResult.stderr,
    interpreter: adapter.interpreter,
    redacted:
      !matcherResult.stdout.includes(redactionProbe) &&
      !matcherResult.stderr.includes(redactionProbe),
  };
}

function wrapNested(value, depth) {
  let nested = value;
  for (let index = 0; index < depth; index += 1) {
    nested = index % 2 === 0 ? { [`level${index}`]: [nested] } : [{ [`level${index}`]: nested }];
  }
  return nested;
}

let historicalRecords;

function materializeHistoricalRecords() {
  historicalRecords ??= adapters.flatMap((adapter) =>
    cases.map((fixture) => ({
      adapter: adapter.name,
      profile: adapter.profile,
      case: fixture.name,
      raw: fixture.raw,
      results: Object.fromEntries(
        adapter.operations.map((operation) => [
          operation,
          runBaseline(adapter, operation, fixture.raw),
        ]),
      ),
    })),
  );
  return historicalRecords;
}

test("historical provider matchers materialize 112 fixed differential cases", () => {
  const records = materializeHistoricalRecords();

  assert.equal(records.length, 112);
  assert.equal(new Set(records.map(({ adapter, case: name }) => `${adapter}:${name}`)).size, 112);
  for (const record of records) {
    for (const result of Object.values(record.results)) {
      assert.equal(typeof result.status, "number");
      assert.equal(typeof result.stdout, "string");
      assert.equal(typeof result.stderr, "string");
      assert.ok(result.interpreter);
      assert.equal(result.redacted, true);
    }
  }

  const malformed = records.filter(({ case: name }) => name === "malformed-json");
  assert.equal(malformed.length, 7);
  assert.equal(malformed.find(({ adapter }) => adapter === "github-codespaces").results.probe.status, 2);
  assert.match(malformed[0].results.contains.stderr, /^invalid JSON: /);
});

test("shared matcher reproduces all 112 historical adapter cases", () => {
  for (const record of materializeHistoricalRecords()) {
    const adapter = adapters.find(({ name }) => name === record.adapter);
    for (const operation of adapter.operations) {
      assert.deepEqual(
        runCandidate(adapter, operation, record.raw),
        record.results[operation],
        `${record.adapter}:${record.case}:${operation}`,
      );
    }
  }
});

test("historical provider matchers traverse the generated nested JSON corpus", () => {
  const nestedCases = adapters.flatMap((adapter) =>
    Array.from({ length: 8 }, (_, index) => ({
      adapter,
      depth: index + 1,
      raw: JSON.stringify(wrapNested({ labels: { slug } }, index + 1)),
    })),
  );

  assert.equal(nestedCases.length, 56);
  for (const fixture of nestedCases) {
    const result = runBaseline(fixture.adapter, "contains", fixture.raw);
    assert.equal(result.status, 0, `${fixture.adapter.name} depth=${fixture.depth}: ${result.stderr}`);
    assert.equal(result.stdout, "");
    assert.equal(result.stderr, "");
  }
});

test("shared matcher traverses the generated nested JSON corpus", () => {
  for (const adapter of adapters) {
    for (let depth = 1; depth <= 8; depth += 1) {
      const raw = JSON.stringify(wrapNested({ labels: { slug } }, depth));
      const expected = runBaseline(adapter, "contains", raw);
      const actual = runCandidate(adapter, "contains", raw);
      assert.deepEqual(actual, expected, `${adapter.name} depth=${depth}`);
    }
  }
});

test("shared matcher accepts only the five closed profiles", () => {
  for (const profile of ["standard", "runpod", "lambda", "nebius", "vast"]) {
    const result = runMatcher(profile, "probe", JSON.stringify({ labels: { slug } }));
    assert.equal(result.status, 0, `${profile}: ${result.stderr}`);
  }

  const invalid = runMatcher("custom", "probe", "[]");
  assert.equal(invalid.status, 2);
  assert.equal(invalid.stdout, "");
  assert.equal(invalid.stderr, "usage: live-smoke-json-match.py PROFILE OPERATION\n");
});
