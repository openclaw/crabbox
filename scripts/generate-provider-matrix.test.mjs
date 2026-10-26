import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { renderProviderCategories, validateReadmeCategories } from "./generate-provider-matrix.mjs";

const metadata = JSON.parse(fs.readFileSync(new URL("../docs/providers/provider-metadata.json", import.meta.url), "utf8"));
const readme = fs.readFileSync(new URL("../README.md", import.meta.url), "utf8");
const providerReadme = fs.readFileSync(new URL("../docs/providers/README.md", import.meta.url), "utf8");

function categorySection(source, heading) {
  return source.split(heading)[1]?.split(/^(?:##? |<!-- END GENERATED)/m)[0] ?? "";
}

test("serve is listed as self-hosted virtualization in the README", () => {
  const section = readme.split("### Self-hosted virtualization")[1]?.split(/^### /m)[0] ?? "";
  assert.match(section, /\[serve\]\(docs\/providers\/serve\.md\)/);
});

test("the provider category index includes serve in its metadata category", () => {
  const section = categorySection(providerReadme, "## Providers By Category");
  const row = section.split("\n").find((line) => line.startsWith("| `self-hosted-virtualization` |"));
  assert.match(row ?? "", /\[serve\]\(serve\.md\)/);
});

test("the category index matches metadata membership and counts", () => {
  const section = categorySection(providerReadme, "## Providers By Category");
  assert.equal(`## Providers By Category${section}`.trim(), renderProviderCategories(metadata));
});

test("all README providers match their metadata categories", () => {
  validateReadmeCategories(readme, metadata);
});

const fixtureMetadata = {
  alpha: { category: "self-hosted-virtualization", docs: "alpha.md" },
  beta: { category: "local-runtime", docs: "beta.md" }
};
const fixtureReadme = `<!-- BEGIN README PROVIDER LIST -->
### Self-hosted
Catalog category: \`self-hosted-virtualization\`.
| Provider | Best fit |
| --- | --- |
| [alpha](docs/providers/alpha.md) | Own machines. |
### Local
Catalog category: \`local-runtime\`.
| Provider | Best fit |
| --- | --- |
| [beta](docs/providers/beta.md) | Local containers. |
<!-- END README PROVIDER LIST -->`;

test("README validation rejects wrong categories, omissions, duplicates, and bad links", async (t) => {
  validateReadmeCategories(fixtureReadme, fixtureMetadata);
  const cases = [
    ["wrong category", fixtureReadme.replace("[alpha](docs/providers/alpha.md)", "[beta](docs/providers/beta.md)"), /lists beta under self-hosted-virtualization; metadata category is local-runtime/],
    ["missing provider", fixtureReadme.replace(/^\| \[alpha\].*\n/m, ""), /provider list is missing: alpha/],
    ["duplicate provider", fixtureReadme.replace("### Local", "| [alpha](docs/providers/alpha.md) | Duplicate. |\n### Local"), /lists provider alpha more than once/],
    ["wrong link", fixtureReadme.replace("docs/providers/alpha.md", "docs/providers/beta.md"), /incorrect docs link for alpha/],
    ["missing category", fixtureReadme.replace("Catalog category: `local-runtime`.", ""), /unknown or missing catalog category/],
    ["unknown provider", fixtureReadme.replace("[alpha]", "[unknown]"), /lists unknown provider unknown/]
  ];
  for (const [name, source, error] of cases) {
    await t.test(name, () => assert.throws(() => validateReadmeCategories(source, fixtureMetadata), error));
  }
});
