#!/usr/bin/env node
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

const root = process.cwd();
const docsDir = path.join(root, "docs");
const outDir = path.join(root, "dist", "docs-site");
const repoEditBase = "https://github.com/openclaw/crabbox/edit/main/docs";
const customDomain = "crabbox.sh";
const providerMetadata = JSON.parse(
  fs.readFileSync(path.join(docsDir, "providers", "provider-metadata.json"), "utf8"),
);
const providerMetadataByDocs = new Map(
  Object.entries(providerMetadata).map(([name, metadata]) => [metadata.docs, { name, metadata }]),
);
const legacyProviderFeatureNotes = new Set([
  "aws.md",
  "azure.md",
  "aws-private-workspaces.md",
  "blacksmith-testbox.md",
  "capacity-fallback.md",
  "daytona.md",
  "delegated-runner-contract.md",
  "e2b.md",
  "hetzner.md",
  "islo.md",
  "namespace-devbox.md",
  "namespace-devbox-setup.md",
  "provider-authoring.md",
  "provider-landscape.md",
  "provider-live-smoke.md",
  "provider-selection.md",
  "providers.md",
  "semaphore.md",
  "slurm-academic-sandboxes.md",
  "sprites.md",
]);

const sections = [
  [
    "Start",
    [
      "README.md",
      "getting-started.md",
      "use-cases.md",
      "pricing.md",
      "how-it-works.md",
      "architecture.md",
      "orchestrator.md",
      "cli.md",
    ],
  ],
  ["Integrations", rels("integrations")],
  ["Providers", rels("providers")],
  ["Features", rels("features")],
  ["Commands", rels("commands")],
  [
    "Operate",
    ["operations.md", "observability.md", "troubleshooting.md", "performance.md", "infrastructure.md", "security.md"],
  ],
];

fs.rmSync(outDir, { recursive: true, force: true });
fs.mkdirSync(outDir, { recursive: true });

const pages = allMarkdown(docsDir).map((file) => {
  const rel = path.relative(docsDir, file).replaceAll(path.sep, "/");
  const markdown = fs.readFileSync(file, "utf8");
  const title = firstHeading(markdown) || titleize(path.basename(rel, ".md"));
  return { file, rel, title, outRel: outPath(rel), markdown };
});

const pageMap = new Map(pages.map((page) => [page.rel, page]));
const nav = sections
  .map(([name, rels]) => ({
    name,
    pages: rels.map((rel) => pageMap.get(rel)).filter(Boolean),
  }))
  .filter((section) => section.pages.length);

const sectionByRel = new Map();
for (const section of nav) for (const page of section.pages) sectionByRel.set(page.rel, section.name);
const orderedPages = nav.flatMap((s) => s.pages);

for (const page of pages) {
  const html = markdownToHtml(page.markdown, page.rel);
  const toc = tocFromHtml(html);
  const idx = orderedPages.findIndex((p) => p.rel === page.rel);
  const prev = idx > 0 ? orderedPages[idx - 1] : null;
  const next = idx >= 0 && idx < orderedPages.length - 1 ? orderedPages[idx + 1] : null;
  const sectionName = sectionByRel.get(page.rel) || "Crabbox docs";
  const pageOut = path.join(outDir, page.outRel);
  fs.mkdirSync(path.dirname(pageOut), { recursive: true });
  fs.writeFileSync(pageOut, layout({ page, html, toc, prev, next, sectionName }), "utf8");
}

fs.writeFileSync(path.join(outDir, "crabbox.svg"), crabSvg(), "utf8");
fs.writeFileSync(path.join(outDir, ".nojekyll"), "", "utf8");
fs.writeFileSync(path.join(outDir, "llms.txt"), llmsTxt(), "utf8");
writeAgentSkillsDiscovery();
writeAgentMap();
console.log(`built docs site: ${path.relative(root, outDir)}`);

function writeAgentSkillsDiscovery() {
  const sourcePath = path.join(root, "skills", "crabbox", "SKILL.md");
  const skill = fs.readFileSync(sourcePath, "utf8");
  const frontmatter = skill.match(/^---\n([\s\S]*?)\n---\n/);
  if (!frontmatter) throw new Error(`${path.relative(root, sourcePath)} has no YAML frontmatter`);

  const name = frontmatter[1].match(/^name:\s*([a-z0-9-]+)$/m)?.[1];
  const encodedDescription = frontmatter[1].match(/^description:\s*("(?:\\.|[^"\\])*")$/m)?.[1];
  if (!name || !encodedDescription) {
    throw new Error(`${path.relative(root, sourcePath)} must declare a quoted description and name`);
  }
  const description = JSON.parse(encodedDescription);
  const digest = crypto.createHash("sha256").update(skill).digest("hex");
  const discoveryDir = path.join(outDir, ".well-known", "agent-skills");
  const publishedSkillDir = path.join(discoveryDir, name);
  fs.mkdirSync(publishedSkillDir, { recursive: true });
  fs.writeFileSync(path.join(publishedSkillDir, "SKILL.md"), skill, "utf8");
  fs.writeFileSync(
    path.join(discoveryDir, "index.json"),
    `${JSON.stringify(
      {
        $schema: "https://schemas.agentskills.io/discovery/0.2.0/schema.json",
        skills: [
          {
            name,
            type: "skill-md",
            description,
            url: `/.well-known/agent-skills/${name}/SKILL.md`,
            digest: `sha256:${digest}`,
          },
        ],
      },
      null,
      2,
    )}\n`,
    "utf8",
  );
  writeAICatalog({ name, description });
}

function writeAICatalog({ name, description }) {
  const origin = docsOrigin();
  if (!origin) throw new Error("Agentic Resource Discovery requires a canonical docs origin");
  const catalog = {
    specVersion: "1.0",
    host: {
      displayName: "Crabbox",
      documentationUrl: `${origin}/integrations/agents.html`,
    },
    entries: [
      {
        identifier: `urn:air:crabbox.sh:skill:${name}`,
        displayName: "Crabbox Agent Skill",
        // Current AI Catalog integrated-ecosystem type. ARD's draft examples
        // and bundled conformance helper still disagree on older alternatives.
        type: "application/agent-skills+md",
        url: `${origin}/.well-known/agent-skills/${name}/SKILL.md`,
        description,
        tags: ["remote-testing", "remote-execution", "developer-tools", "agent-skill"],
        capabilities: [
          "RemoteTestExecution",
          "ReusableRemoteEnvironment",
          "CrossPlatformValidation",
          "AuditableExecutionEvidence",
        ],
        representativeQueries: [
          "run this repository's tests on a clean remote machine",
          "validate this change on Linux, macOS, or Windows",
          "use Crabbox to collect auditable remote test evidence",
        ],
      },
    ],
  };
  fs.writeFileSync(
    path.join(outDir, ".well-known", "ai-catalog.json"),
    `${JSON.stringify(catalog, null, 2)}\n`,
    "utf8",
  );
}

function writeAgentMap() {
  const origin = docsOrigin();
  if (!origin) return;
  fs.writeFileSync(
    path.join(outDir, "robots.txt"),
    `User-agent: *\nAllow: /\nAgentmap: ${origin}/.well-known/ai-catalog.json\n`,
    "utf8",
  );
}

function llmsTxt() {
  const origin = docsOrigin();
  const source = docsSourceUrl();
  const name = typeof productName !== "undefined" ? productName : path.basename(root);
  const description = typeof productDescription !== "undefined" ? productDescription : `${name} documentation index.`;
  const install = docsInstallHint();
  const docPages = docsLlmsPages().map((page) => `- ${page.title}: ${pageUrl(origin, page.outRel)}`);
  const lines = [
    `# ${name}`,
    "",
    description,
    "",
    "Canonical documentation:",
    ...docPages,
  ];
  if (install) {
    lines.push("", "Install:", `- ${install}`);
  }
  if (source) {
    lines.push("", `Source: ${source}`);
  }
  if (origin) {
    lines.push(
      "",
      "Agent Skill and resource discovery:",
      `- ${origin}/.well-known/agent-skills/index.json`,
      `- ${origin}/.well-known/agent-skills/crabbox/SKILL.md`,
      `- ${origin}/.well-known/ai-catalog.json`,
    );
  }
  lines.push("", "Guidance for agents:", "- Prefer the canonical documentation URLs above over README excerpts or package metadata.", "- Fetch only the pages needed for the current task; this is an index, not a full-site corpus.");
  return `${lines.join("\n")}\n`;
}

function docsLlmsPages() {
  const seen = new Set();
  const ordered = typeof orderedPages !== "undefined" ? orderedPages : [];
  return [...ordered, ...pages].filter((page) => page.outRel && !seen.has(page.outRel) && seen.add(page.outRel));
}

function docsOrigin() {
  const value =
    (typeof siteBase !== "undefined" && siteBase) ||
    (typeof siteUrl !== "undefined" && siteUrl) ||
    (typeof customDomain !== "undefined" && customDomain ? `https://${customDomain}` : "");
  return value.replace(/\/$/, "");
}

function docsSourceUrl() {
  if (typeof repoBase !== "undefined") return repoBase;
  if (typeof repoUrl !== "undefined") return repoUrl;
  if (typeof repoEditBase !== "undefined") return repoEditBase.replace(/\/edit\/main\/docs\/?$/, "");
  return "";
}

function docsInstallHint() {
  if (typeof installCommand !== "undefined") return installCommand;
  if (typeof installLine !== "undefined") return installLine;
  if (typeof installCmd !== "undefined") return installCmd;
  if (typeof installSnippet !== "undefined") return installSnippet;
  if (typeof brewInstall !== "undefined") return brewInstall;
  return "";
}

function pageUrl(origin, outRel) {
  const normalized = outRel === "index.html" ? "" : outRel.replace(/(?:^|\/)index\.html$/, (match) => match === "index.html" ? "" : "/");
  if (!origin) return normalized || "index.html";
  return normalized ? `${origin}/${normalized}` : `${origin}/`;
}

function rels(dir) {
  const full = path.join(docsDir, dir);
  if (!fs.existsSync(full)) return [];
  return fs
    .readdirSync(full)
    .filter((name) => name.endsWith(".md") && !(dir === "features" && legacyProviderFeatureNotes.has(name)))
    .sort((a, b) => (a === "README.md" ? -1 : b === "README.md" ? 1 : a.localeCompare(b)))
    .map((name) => `${dir}/${name}`);
}

function allMarkdown(dir) {
  return fs
    .readdirSync(dir, { withFileTypes: true })
    .flatMap((entry) => {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) return allMarkdown(full);
      return entry.name.endsWith(".md") ? [full] : [];
    })
    .sort();
}

function outPath(rel) {
  if (rel === "README.md") return "index.html";
  if (rel.endsWith("/README.md")) return rel.replace(/README\.md$/, "index.html");
  return rel.replace(/\.md$/, ".html");
}

function firstHeading(markdown) {
  return markdown.match(/^#\s+(.+)$/m)?.[1]?.trim();
}

function titleize(input) {
  return input.replaceAll("-", " ").replace(/\b\w/g, (m) => m.toUpperCase());
}

export function markdownToHtml(markdown, currentRel) {
  const lines = markdown.replace(/\r\n/g, "\n").split("\n");
  const html = [];
  let paragraph = [];
  let list = null;
  let fence = null;
  let htmlComment = false;

  const flushParagraph = () => {
    if (!paragraph.length) return;
    html.push(`<p>${inline(paragraph.join(" "), currentRel)}</p>`);
    paragraph = [];
  };
  const closeList = () => {
    if (!list) return;
    if (list.current) list.items.push(list.current);
    html.push(
      `<${list.tag}>${list.items
        .map((item) => `<li>${inline(item, currentRel)}</li>`)
        .join("")}</${list.tag}>`,
    );
    list = null;
  };
  const splitRow = (line) => line.replace(/^\s*\|/, "").replace(/\|\s*$/, "").split("|").map((s) => s.trim());
  const isDivider = (line) => /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)+\|?\s*$/.test(line);

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const fenceMatch = line.match(/^```(\w+)?\s*$/);
    if (fenceMatch) {
      flushParagraph();
      closeList();
      if (fence) {
        html.push(`<pre><code class="language-${fence.lang}">${escapeHtml(fence.lines.join("\n"))}</code></pre>`);
        fence = null;
      } else {
        fence = { lang: fenceMatch[1] || "text", lines: [] };
      }
      continue;
    }
    if (fence) {
      fence.lines.push(line);
      continue;
    }
    if (htmlComment) {
      if (line.includes("-->")) htmlComment = false;
      continue;
    }
    if (line.trimStart().startsWith("<!--")) {
      flushParagraph();
      closeList();
      htmlComment = !line.includes("-->");
      continue;
    }
    if (!line.trim()) {
      flushParagraph();
      closeList();
      continue;
    }
    const heading = line.match(/^(#{1,4})\s+(.+)$/);
    if (heading) {
      flushParagraph();
      closeList();
      const level = heading[1].length;
      const text = heading[2].trim();
      const id = slug(text);
      const inner = inline(text, currentRel);
      if (level === 1) {
        html.push(`<h1 id="${id}">${inner}</h1>`);
      } else {
        html.push(`<h${level} id="${id}"><a class="anchor" href="#${id}" aria-label="Anchor link">#</a>${inner}</h${level}>`);
      }
      continue;
    }
    if (line.trimStart().startsWith("|") && line.includes("|", line.indexOf("|") + 1) && isDivider(lines[i + 1] || "")) {
      flushParagraph();
      closeList();
      const header = splitRow(line);
      const aligns = splitRow(lines[i + 1]).map((cell) => {
        const left = cell.startsWith(":");
        const right = cell.endsWith(":");
        return right && left ? "center" : right ? "right" : left ? "left" : "";
      });
      i += 1;
      const rows = [];
      while (i + 1 < lines.length && lines[i + 1].trimStart().startsWith("|")) {
        i += 1;
        rows.push(splitRow(lines[i]));
      }
      const isProviderMatrix = currentRel === "providers/README.md" && header[0] === "Provider";
      const th = header.map((c, idx) => `<th${aligns[idx] ? ` style="text-align:${aligns[idx]}"` : ""}>${inline(c, currentRel)}</th>`).join("");
      const tb = rows.map((r) => {
        const attrs = isProviderMatrix ? providerRowAttributes(r) : "";
        const cells = r.map((c, idx) => `<td${aligns[idx] ? ` style="text-align:${aligns[idx]}"` : ""}>${inline(c, currentRel)}</td>`).join("");
        return `<tr${attrs}>${cells}</tr>`;
      }).join("");
      const tableClass = isProviderMatrix ? ' class="provider-matrix"' : "";
      const tableLabel = `${stripHtmlTags(inline(header[0], currentRel)) || "Data"} table`;
      if (isProviderMatrix) html.push(providerFilterHtml(rows.length));
      html.push(`<div class="table-scroll" role="region" aria-label="${escapeAttr(tableLabel)}" tabindex="0"><table${tableClass}><thead><tr>${th}</tr></thead><tbody>${tb}</tbody></table></div>`);
      continue;
    }
    const bullet = line.match(/^\s*-\s+(.+)$/);
    const numbered = line.match(/^\s*\d+\.\s+(.+)$/);
    if (bullet || numbered) {
      flushParagraph();
      const tag = bullet ? "ul" : "ol";
      if (list && list.tag !== tag) closeList();
      if (!list) {
        list = { tag, items: [], current: "" };
      }
      if (list.current) list.items.push(list.current);
      list.current = (bullet || numbered)[1];
      continue;
    }
    if (list && /^\s{2,}\S/.test(line)) {
      list.current += ` ${line.trim()}`;
      continue;
    }
    closeList();
    paragraph.push(line.trim());
  }
  flushParagraph();
  closeList();
  return html.join("\n");
}

function providerRowAttributes(row) {
  const docsMatch = row[0]?.match(/\]\(([^)#]+\.md)(?:#[^)]+)?\)/);
  const docs = docsMatch?.[1] || "";
  const entry = providerMetadataByDocs.get(docs);
  const name = entry?.name || path.basename(docs, ".md");
  const metadata = entry?.metadata || {};
  const search = [name, ...row, ...Object.values(metadata)]
    .filter((value) => typeof value === "string")
    .join(" ")
    .replace(/[\[\]()`*_]/g, " ")
    .replace(/\s+/g, " ")
    .trim()
    .toLowerCase();
  return ` data-provider="${escapeAttr(name)}" data-provider-groups="${escapeAttr(providerGroups(metadata).join(" "))}" data-provider-search="${escapeAttr(search)}"`;
}

function providerGroups(metadata) {
  const groups = [primaryProviderGroup(metadata.category)];
  if (metadata.coordinator === true && !groups.includes("team-cloud")) groups.push("team-cloud");
  return groups;
}

function primaryProviderGroup(category) {
  if (category === "brokerable-cloud") return "team-cloud";
  if (category === "direct-cloud") return "managed-cloud";
  if (category === "delegated-sandbox") return "sandboxes";
  if (category === "gpu-cloud") return "gpu";
  if (["local-sandbox", "local-runtime", "local-vm"].includes(category)) return "local";
  if (["self-hosted-virtualization", "external-provider", "byo-ssh"].includes(category)) return "self-hosted";
  if (["ci-proof-runner", "service-control"].includes(category)) return "ci-services";
  return "other";
}

function providerFilterHtml(count) {
  const groups = [
    ["all", "All"],
    ["team-cloud", "Team cloud"],
    ["managed-cloud", "Managed cloud"],
    ["sandboxes", "Sandboxes"],
    ["gpu", "GPU & ML"],
    ["local", "Local"],
    ["self-hosted", "Self-hosted & BYO"],
    ["ci-services", "CI & services"],
  ];
  return `<div class="provider-filter" data-provider-filter>
  <div class="provider-filter-head">
    <label for="provider-filter-input">Find a provider</label>
    <output for="provider-filter-input" aria-live="polite" data-provider-count>${count} providers</output>
  </div>
  <input id="provider-filter-input" name="provider-filter" type="search" autocomplete="off" spellcheck="false" placeholder="Try GPU, local, macOS…">
  <div class="provider-filter-groups" role="group" aria-label="Provider category">
    ${groups.map(([value, label], index) => `<button type="button" data-provider-group-filter="${value}" aria-pressed="${index === 0 ? "true" : "false"}">${label}</button>`).join("")}
  </div>
  <p class="provider-empty" role="status" hidden data-provider-empty>No providers match these filters.</p>
</div>`;
}

function inline(text, currentRel) {
  const stash = [];
  let out = text.replace(/`([^`]+)`/g, (_, code) => {
    stash.push(`<code>${escapeHtml(code)}</code>`);
    return `\u0000${stash.length - 1}\u0000`;
  });
  out = escapeHtml(out)
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_, label, href) => `<a href="${escapeAttr(rewriteHref(href, currentRel))}">${label}</a>`);
  return out.replace(/\u0000(\d+)\u0000/g, (_, i) => stash[Number(i)]);
}

function rewriteHref(href, currentRel) {
  if (/^(https?:|mailto:|#)/.test(href)) return href;
  const [raw, hash = ""] = href.split("#");
  if (!raw) return `#${hash}`;
  if (!raw.endsWith(".md")) return href;
  const from = path.posix.dirname(currentRel);
  const target = path.posix.normalize(path.posix.join(from, raw));
  let rewritten = outPath(target);
  const currentOut = outPath(currentRel);
  rewritten = path.posix.relative(path.posix.dirname(currentOut), rewritten) || "index.html";
  return `${rewritten}${hash ? `#${hash}` : ""}`;
}

function tocFromHtml(html) {
  const items = [];
  let cursor = 0;
  while (cursor < html.length) {
    const h2 = html.indexOf('<h2 id="', cursor);
    const h3 = html.indexOf('<h3 id="', cursor);
    const open = firstIndex(h2, h3);
    if (open < 0) break;
    const level = open === h2 ? 2 : 3;
    const idStart = open + `<h${level} id="`.length;
    const idEnd = html.indexOf('"', idStart);
    const bodyStart = html.indexOf(">", idEnd);
    const closeTag = `</h${level}>`;
    const close = html.indexOf(closeTag, bodyStart + 1);
    if (idEnd < 0 || bodyStart < 0 || close < 0) break;
    const body = html.slice(bodyStart + 1, close);
    const text = stripHtmlTags(stripHeadingAnchor(body)).trim();
    items.push({ level, id: html.slice(idStart, idEnd), text });
    cursor = close + closeTag.length;
  }
  if (items.length < 2) return "";
  return `<nav class="toc" aria-label="On this page"><h2>On this page</h2>${items
    .map((i) => `<a class="toc-l${i.level}" href="#${i.id}">${escapeHtml(i.text)}</a>`)
    .join("")}</nav>`;
}

function layout({ page, html, toc, prev, next, sectionName }) {
  const depth = page.outRel.split("/").length - 1;
  const rootPrefix = depth ? "../".repeat(depth) : "";
  const editUrl = `${repoEditBase}/${page.rel}`;
  const isHome = page.rel === "README.md";
  const isProviderIndex = page.rel === "providers/README.md";
  const documentTitle = isHome
    ? "Crabbox — Run Any Repository Command in the Right Box"
    : `${page.title} - Crabbox Docs`;
  const metaDescription = isHome
    ? "Run repository commands in local sandboxes, cloud VMs, SSH hosts, Windows and WSL2, macOS, or hosted agent sandboxes through one CLI."
    : `${page.title} documentation for Crabbox remote execution.`;
  const canonicalUrl = pageUrl(docsOrigin(), page.outRel);
  const prevNext = !isHome && (prev || next) ? pageNavHtml(prev, next, rootPrefix) : "";
  const heroBlock = isHome ? landingHero(rootPrefix) : standardHero(page, sectionName, editUrl);
  const articleClass = isProviderIndex ? "doc doc-wide" : "doc";
  const tocBlock = isProviderIndex ? "" : toc;
  const articleBlock = isHome
    ? ""
    : `<div class="doc-grid${isProviderIndex ? " doc-grid-wide" : ""}">
        <article class="${articleClass}">${html}${prevNext}</article>
        ${tocBlock}
      </div>`;
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <meta name="theme-color" id="theme-color" content="#f4f5f5">
  <title>${escapeHtml(documentTitle)}</title>
  <meta name="description" content="${escapeAttr(metaDescription)}">
  <link rel="canonical" href="${escapeAttr(canonicalUrl)}">
  <meta property="og:type" content="website">
  <meta property="og:site_name" content="Crabbox">
  <meta property="og:title" content="${escapeAttr(documentTitle)}">
  <meta property="og:description" content="${escapeAttr(metaDescription)}">
  <meta property="og:url" content="${escapeAttr(canonicalUrl)}">
  <link rel="icon" href="${rootPrefix}crabbox.svg">
  <link rel="ai-catalog" href="/.well-known/ai-catalog.json" type="application/ai-catalog+json">
  <script>(function(){var s;try{s=localStorage.getItem('crabbox-docs-theme')}catch(e){}var d=window.matchMedia&&matchMedia('(prefers-color-scheme: dark)').matches;var t=(s==='light'||s==='dark')?s:(d?'dark':'light');document.documentElement.dataset.theme=t;document.getElementById('theme-color').content=t==='dark'?'#17191b':'#f4f5f5'})();</script>
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=Fraunces:wght@600;700&family=IBM+Plex+Sans:wght@400;500;600;700&family=IBM+Plex+Mono:wght@500;600&display=swap" rel="stylesheet">
  <style>${css()}</style>
</head>
<body${isHome ? ' class="home"' : ""}>
  <a class="skip-link" href="#main-content">Skip to content</a>
  <button class="nav-toggle" type="button" aria-label="Open navigation" aria-controls="docs-sidebar" aria-expanded="false">
    <span aria-hidden="true"></span><span aria-hidden="true"></span><span aria-hidden="true"></span>
  </button>
  <button class="nav-backdrop" type="button" aria-label="Close navigation" aria-hidden="true" hidden></button>
  <div class="shell">
    <aside class="sidebar" id="docs-sidebar" tabindex="-1">
      <div class="sidebar-head">
        <a class="brand" href="${rootPrefix}index.html" aria-label="Crabbox docs home">
          <img src="${rootPrefix}crabbox.svg" alt="" width="42" height="42">
          <span><strong>Crabbox</strong><small>Execution control plane</small></span>
        </a>
        ${themeToggleHtml()}
      </div>
      <label class="search"><span>Search docs</span><input id="doc-search" name="docs-search" type="search" autocomplete="off" spellcheck="false" placeholder="Provider, command, topic…"></label>
      <nav class="sidebar-nav" aria-label="Documentation">${navHtml(page.rel, rootPrefix)}</nav>
      <p class="nav-empty" role="status" aria-live="polite" hidden>No matching pages.</p>
      <div class="sidebar-foot"><a href="https://github.com/openclaw/crabbox" rel="noopener">GitHub repository</a></div>
    </aside>
    <main id="main-content" tabindex="-1">
      ${heroBlock}
      ${articleBlock}
    </main>
  </div>
  <script>${js()}</script>
</body>
</html>`;
}

function standardHero(page, sectionName, editUrl) {
  return `<header class="hero">
        <div class="hero-text">
          <p class="eyebrow">${escapeHtml(sectionName)}</p>
          <h1>${escapeHtml(page.title)}</h1>
        </div>
        <div class="hero-meta">
          <a class="repo" href="https://github.com/openclaw/crabbox" rel="noopener">GitHub</a>
          <a class="edit" href="${escapeAttr(editUrl)}" rel="noopener">Edit page</a>
        </div>
      </header>`;
}

function landingHero(rootPrefix) {
  const useCases = [
    {
      id: "fast-feedback",
      number: "01",
      title: "Fast Test Loops",
      summary: "Warm boxes and reusable caches",
      resultTitle: "Shorten the edit–run loop.",
      resultBody: "Start with providers that favor local execution, reusable caches, warm sessions, or other fast-feedback paths.",
      commands: ["crabbox providers recommend fast-feedback", "crabbox doctor --provider <name>"],
      href: "use-cases.html#speed-up-test-and-build-loops",
      linkLabel: "Open the Fast Test Loop Guide",
    },
    {
      id: "agent-sandbox",
      number: "02",
      title: "Coding Agents",
      summary: "Agent workspaces and cleanup routes",
      resultTitle: "Separate agent fit from cleanup guarantees.",
      resultBody: "Compare agent- and devbox-shaped runtimes with the stricter disposable route, then inspect the selected provider’s isolation, network, secret, and cleanup boundary.",
      commands: [
        "crabbox providers recommend agent-sandbox",
        "crabbox providers recommend disposable-execution",
        "crabbox doctor --provider <name>",
      ],
      href: "use-cases.html#give-a-coding-agent-a-disposable-environment",
      linkLabel: "Open the Coding Agent Guide",
    },
    {
      id: "cross-platform",
      number: "03",
      title: "Cross-Platform",
      summary: "Linux, Windows, WSL2, macOS",
      resultTitle: "Match the operating system before the vendor.",
      resultBody: "Compare the platform routes independently; Windows includes native and WSL2-capable paths, subject to the selected host.",
      commands: [
        "crabbox providers recommend linux-vm",
        "crabbox providers recommend windows",
        "crabbox providers recommend macos",
      ],
      href: "use-cases.html#validate-linux-windows-wsl2-or-macos",
      linkLabel: "Open the Platform Validation Guide",
    },
    {
      id: "desktop",
      number: "04",
      title: "Browser & Desktop QA",
      summary: "Visible browser and desktop sessions",
      resultTitle: "Start with a visible desktop-capable target.",
      resultBody: "Compare browser and desktop paths first, then add evidence routing when screenshots, video, or diagnostics must survive the lease.",
      commands: ["crabbox providers recommend desktop", "crabbox doctor --provider <name>"],
      href: "use-cases.html#test-a-browser-or-visible-desktop",
      linkLabel: "Open the Browser & Desktop Guide",
    },
    {
      id: "fanout-testing",
      number: "05",
      title: "Parallel Experiments",
      summary: "Parallel runs from prepared state",
      resultTitle: "Fan prepared state out across multiple attempts.",
      resultBody: "Start with checkpoint- and fork-capable workspace providers for test shards, branch comparisons, or best-of-N runs.",
      commands: ["crabbox providers recommend fanout-testing", "crabbox doctor --provider <name>"],
      href: "use-cases.html#fan-out-parallel-experiments",
      linkLabel: "Open the Parallel Experiments Guide",
    },
    {
      id: "gpu",
      number: "06",
      title: "GPU Workloads",
      summary: "GPU machines and sandboxes",
      resultTitle: "Require GPU capability before choosing capacity.",
      resultBody: "Compare SSH-accessible and delegated accelerator paths for model tests, CUDA builds, rendering, or other GPU-shaped work.",
      commands: ["crabbox providers recommend gpu", "crabbox doctor --provider <name>"],
      href: "use-cases.html#run-gpu-workloads",
      linkLabel: "Open the GPU Workload Guide",
    },
  ];
  const jobChoices = useCases
    .map(
      ({ id, number, title, summary }, index) =>
        `<div class="home-job-option">
          <input class="home-job-radio sr-only" type="radio" name="home-job" id="home-job-${escapeAttr(id)}" value="${escapeAttr(id)}" aria-controls="home-job-result-${escapeAttr(id)}" data-home-job-radio${index === 0 ? " checked" : ""}>
          <label for="home-job-${escapeAttr(id)}"><span>${number}</span><strong>${escapeHtml(title)}</strong><small>${escapeHtml(summary)}</small></label>
        </div>`,
    )
    .join("");
  const jobResults = useCases
    .map(({ id, resultTitle, resultBody, commands, href, linkLabel }, index) => {
      const recommendationCommands = commands.filter((command) => !command.startsWith("crabbox doctor "));
      const copyLabel = recommendationCommands.length === 1 ? "Copy recommendation command" : "Copy recommendation commands";
      return `<article class="home-job-result" id="home-job-result-${escapeAttr(id)}" data-home-job-result="${escapeAttr(id)}" aria-labelledby="home-job-result-title-${escapeAttr(id)}"${index === 0 ? ' data-home-job-default="true"' : ""}>
          <div class="home-job-result-copy">
            <p class="eyebrow">Recommended Starting Path</p>
            <h3 id="home-job-result-title-${escapeAttr(id)}">${escapeHtml(resultTitle)}</h3>
            <p>${escapeHtml(resultBody)}</p>
          </div>
          <div class="home-job-command">
            <div><span>built-in recommendations</span><small>verify after selection</small></div>
            <pre data-copyable data-copy-text="${escapeAttr(recommendationCommands.join("\n"))}" data-copy-label="${copyLabel}"><code>${commands.map((command) => `<span class="home-job-command-line"><i aria-hidden="true">$</i><b>${escapeHtml(command)}</b></span>`).join("")}</code></pre>
            <a href="${rootPrefix}${href}">${escapeHtml(linkLabel)} <i aria-hidden="true">→</i></a>
          </div>
        </article>`;
    })
    .join("");
  const paths = [
    [
      "Local",
      "Stay on this machine",
      "Containers, full VMs, and policy sandboxes for fast checks without cloud credentials.",
      "use-cases.html#run-locally-without-cloud-credentials",
      "See Local Paths",
    ],
    [
      "Your Accounts and Infrastructure",
      "Use capacity you control",
      "Cloud accounts, SSH hosts, and self-hosted virtualization such as Proxmox or Firecracker.",
      "use-cases.html#use-infrastructure-you-already-own",
      "Use Your Infrastructure",
    ],
    [
      "Provider-Managed",
      "Delegate the runtime",
      "Hosted sandboxes, devboxes, CI proof runners, browser sessions, and GPU jobs.",
      "providers/index.html?q=provider-managed",
      "Browse Managed Providers",
    ],
  ];
  const pathCards = paths
    .map(
      ([eyebrow, title, body, href, label], index) =>
        `<article class="home-path"><span>${String(index + 1).padStart(2, "0")}</span><p>${escapeHtml(eyebrow)}</p><h3>${escapeHtml(title)}</h3><div>${escapeHtml(body)}</div><a href="${rootPrefix}${href}">${escapeHtml(label)} <i aria-hidden="true">→</i></a></article>`,
    )
    .join("");
  const providerCount = Object.keys(providerMetadata).length;
  return `<header class="hero hero-home">
        <div class="home-hero-copy">
          <div class="home-title">
            <img src="${rootPrefix}crabbox.svg" alt="" width="56" height="56" fetchpriority="high">
            <div><strong>Crabbox</strong><small>Remote Execution Control Plane</small></div>
          </div>
          <p class="eyebrow">One CLI. Many Runtimes.</p>
          <h1>Run Your Code in <em>the Right Box.</em></h1>
          <p class="lede">Keep editing locally. Crabbox runs the working tree you have on the machine it needs—a local runtime, cloud VM, SSH host, or managed sandbox—then streams the result back. No bespoke CI job for every iteration.</p>
          <div class="cta">
            <a class="cta-primary" href="${rootPrefix}getting-started.html">Run Your First Command</a>
            <a class="cta-secondary" href="#home-use-cases-heading">Route Your Workload</a>
          </div>
          <ul class="home-facts" aria-label="Crabbox product facts">
            <li>MIT licensed</li>
            <li>Linux, Windows, and macOS</li>
            <li>${providerCount} registered providers</li>
          </ul>
        </div>
        <div class="home-console" role="group" aria-label="Example Crabbox run">
          <div class="home-console-bar"><span aria-hidden="true">● ● ●</span><strong>crabbox / run</strong><small>ready</small></div>
          <pre><code><span>$</span> crabbox run --provider local-container -- pnpm test</code></pre>
          <ol>
            <li><b>01</b><div><strong>Lease</strong><small>blue-lobster · local-container</small></div><i aria-hidden="true">✓</i></li>
            <li><b>02</b><div><strong>Sync</strong><small>tracked + nonignored files</small></div><i aria-hidden="true">✓</i></li>
            <li><b>03</b><div><strong>Run</strong><small>pnpm test · live output</small></div><i aria-hidden="true">✓</i></li>
            <li><b>04</b><div><strong>Release</strong><small>owned container removed</small></div><i aria-hidden="true">✓</i></li>
          </ol>
          <div class="home-console-result"><i aria-hidden="true">✓</i><div><strong>Result returned</strong><small>temporary container cleaned up</small></div></div>
        </div>
      </header>
      <section class="home-section home-use-cases" aria-labelledby="home-use-cases-heading">
        <header><div><p class="eyebrow">Interactive Workload Router</p><h2 id="home-use-cases-heading">Pick the Job. Get the Starting Commands.</h2></div><p>Turn ${providerCount} registered providers into a focused comparison path. Choose a workload, run the matching built-in recommendations, then verify the provider you select with <code>crabbox doctor</code>.</p></header>
        <form class="home-job-finder" data-home-job-finder>
          <fieldset>
            <legend class="sr-only">Choose the job Crabbox should help with</legend>
            <div class="home-job-layout">
              <div class="home-job-choices">${jobChoices}</div>
              <div class="home-job-results">${jobResults}</div>
            </div>
          </fieldset>
          <p class="home-job-disclaimer"><strong>Built-in guidance, not a live provider check.</strong> Availability, price, performance, credentials, quota, and security still depend on the provider. Verify readiness with <code>crabbox doctor</code>.</p>
        </form>
        <a class="home-section-link" href="${rootPrefix}use-cases.html">See All Recommendation Paths <span aria-hidden="true">→</span></a>
      </section>
      <section class="home-section home-paths" aria-labelledby="home-paths-heading">
        <header><div><p class="eyebrow">Choose by Ownership</p><h2 id="home-paths-heading">Run Here, There, or Managed.</h2></div><p>The loop stays the same. Only the runtime owner, isolation boundary, and billing relationship change.</p></header>
        <div class="home-path-grid">${pathCards}</div>
      </section>
      <section class="home-section home-pricing" aria-labelledby="home-pricing-heading">
        <header><div><p class="eyebrow">Current Cost Model</p><h2 id="home-pricing-heading">Crabbox Software Is Free. Compute Isn’t.</h2></div><p>No opaque “box credit” is needed to understand today’s product. Crabbox separates its software from the infrastructure that runs the work.</p></header>
        <div class="home-price-grid">
          <article><span>Crabbox Software</span><strong>$0 license fee</strong><p>MIT-licensed CLI and coordinator.</p></article>
          <article><span>Compute</span><strong>Provider rate</strong><p>Cloud, sandbox, GPU, or local runtime bills remain external.</p></article>
          <article><span>Coordinator</span><strong>Your infrastructure</strong><p>Run it on Cloudflare or Node.js with PostgreSQL.</p></article>
          <article><span>Coordinator Guardrails</span><strong>TTL and spend caps</strong><p>For brokered providers, estimate reserved cost and reject leases over configured limits.</p></article>
        </div>
        <a class="home-section-link" href="${rootPrefix}pricing.html">See Pricing and Cost Boundaries <span aria-hidden="true">→</span></a>
      </section>
      <aside class="home-capability-note" aria-labelledby="home-nested-heading">
        <div><p class="eyebrow">Conditional Capability</p><h2 id="home-nested-heading">Need WSL2 or KVM?</h2></div>
        <p>WSL2 works on compatible AWS and Azure Windows targets. Firecracker requires Crabbox to run on a prepared Linux <code>/dev/kvm</code> host. There is no generic nested mode. <a href="${rootPrefix}features/nested-execution.html">Read the exact boundaries <span aria-hidden="true">→</span></a></p>
      </aside>
      <section class="home-install" aria-labelledby="home-install-heading">
        <div><p class="eyebrow">Install the CLI</p><h2 id="home-install-heading">One Command to Start.</h2><p>Install Crabbox, choose direct, local, delegated, or team-coordinator access, then run the command your repository already knows.</p><a href="${rootPrefix}getting-started.html">Open the 10-Minute Guide <span aria-hidden="true">→</span></a></div>
        <pre><code><span>$</span> brew install openclaw/tap/crabbox
<span>$</span> crabbox doctor
<span>$</span> crabbox run -- pnpm test</code></pre>
      </section>
      <aside class="home-trust" aria-labelledby="home-trust-heading">
        <div><p class="eyebrow">Trust Boundary</p><h2 id="home-trust-heading">Choose Isolation Deliberately.</h2></div>
        <p>Crabbox is a developer execution tool, not one uniform hostile multi-tenant sandbox. Isolation, network policy, secrets, and host access depend on the selected runtime. <a href="${rootPrefix}security.html">Read the security model</a> before running unfamiliar code.</p>
      </aside>`;
}

function pageNavHtml(prev, next, rootPrefix) {
  const cell = (page, dir) => {
    if (!page) return "";
    return `<a class="page-nav-${dir}" href="${rootPrefix}${page.outRel}"><small>${dir === "prev" ? "Previous" : "Next"}</small><span>${escapeHtml(page.title)}</span></a>`;
  };
  return `<nav class="page-nav" aria-label="Pager">${cell(prev, "prev")}${cell(next, "next")}</nav>`;
}

function navHtml(currentRel, rootPrefix) {
  const currentSection = nav.find((section) => section.pages.some((page) => page.rel === currentRel));
  return nav
    .map((section) => {
      const activeSection = section === currentSection;
      const open = activeSection || (!currentSection && section.name === "Start") ? " open" : "";
      return `<details class="nav-section" data-nav-section${open}>
      <summary><h2>${section.name}</h2><span class="nav-count">${section.pages.length}</span><span class="nav-chevron" aria-hidden="true"></span></summary>
      <div class="nav-links">${section.pages.map((page) => {
      const href = rootPrefix + page.outRel;
      const active = page.rel === currentRel ? " active" : "";
      const current = page.rel === currentRel ? ' aria-current="page"' : "";
      const search = `${page.title} ${page.rel} ${path.basename(page.rel, ".md")}`.toLowerCase();
      return `<a class="nav-link${active}" href="${href}" data-nav-search="${escapeAttr(search)}"${current}>${escapeHtml(page.title)}</a>`;
    }).join("")}</div></details>`;
    })
    .join("");
}

function themeToggleHtml() {
  return `<button class="theme-toggle" type="button" aria-label="Toggle dark mode" aria-pressed="false" title="Toggle color theme" data-theme-toggle>
          <svg class="theme-icon-moon" viewBox="0 0 20 20" aria-hidden="true"><path d="M14.6 12.1A6.5 6.5 0 0 1 7.4 2.7a6.5 6.5 0 1 0 7.2 9.4z" fill="currentColor"/></svg>
          <svg class="theme-icon-sun" viewBox="0 0 20 20" aria-hidden="true"><circle cx="10" cy="10" r="3.4" fill="currentColor"/><g stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><line x1="10" y1="2" x2="10" y2="4"/><line x1="10" y1="16" x2="10" y2="18"/><line x1="2" y1="10" x2="4" y2="10"/><line x1="16" y1="10" x2="18" y2="10"/><line x1="4.2" y1="4.2" x2="5.6" y2="5.6"/><line x1="14.4" y1="14.4" x2="15.8" y2="15.8"/><line x1="4.2" y1="15.8" x2="5.6" y2="14.4"/><line x1="14.4" y1="5.6" x2="15.8" y2="4.2"/></g></svg>
        </button>`;
}

function css() {
  return `
:root{color-scheme:light;--ink:#202326;--muted:#626a70;--shell:#f4f5f5;--paper:#fff;--reef:#0a6f6a;--coral:#b94431;--ochre:#a66d0a;--line:#d9dddf;--line-soft:#e8ebec;--panel:#fff;--sidebar:#fafafa;--body-soft:#42484d;--code-bg:#171a1c;--code-fg:#f7f3ed;--code-border:#0f1113;--code-comment:#9aa5a5;--code-scroll:#50575b;--inline-bg:#edf1f1;--inline-border:#dce2e2;--nav-active-bg:#f8e7e3;--nav-active-fg:#7f2e21;--quote-bg:#eef5f4;--focus:#0a6f6a}
:root[data-theme="dark"]{color-scheme:dark;--ink:#f4f0ea;--muted:#aeb5b6;--shell:#17191b;--paper:#202326;--reef:#62c7bd;--coral:#ff8a78;--ochre:#efc15b;--line:#353a3d;--line-soft:#2b2f32;--panel:#202326;--sidebar:#111315;--body-soft:#c7cccb;--code-bg:#0e1011;--code-fg:#f4f0ea;--code-border:#08090a;--code-comment:#8d9999;--code-scroll:#4e565a;--inline-bg:#2b3032;--inline-border:#3b4245;--nav-active-bg:#402722;--nav-active-fg:#ffb3a5;--quote-bg:#1b2d2c;--focus:#62c7bd}
*{box-sizing:border-box}
html{scroll-behavior:smooth;scroll-padding-top:24px}
body{margin:0;background:var(--shell);color:var(--ink);font-family:"IBM Plex Sans",Avenir Next,sans-serif;line-height:1.65;-webkit-font-smoothing:antialiased;transition:background-color .18s,color .18s}
[hidden]{display:none!important}
::selection{background:var(--coral);color:var(--paper)}
a{color:var(--reef);text-decoration-thickness:.07em;text-underline-offset:.18em;transition:color .15s}
a:hover{color:var(--coral)}
button,input{font:inherit}
:is(a,button,input,summary,[tabindex]):focus-visible{outline:3px solid var(--focus);outline-offset:3px}
.skip-link{position:fixed;top:12px;left:12px;z-index:40;padding:8px 12px;background:var(--ink);color:var(--paper);border-radius:4px;text-decoration:none;transform:translateY(-160%)}
.skip-link:focus{transform:translateY(0)}
.sr-only{position:absolute!important;width:1px!important;height:1px!important;padding:0!important;margin:-1px!important;overflow:hidden!important;clip:rect(0,0,0,0)!important;white-space:nowrap!important;border:0!important}
.shell{display:grid;grid-template-columns:296px minmax(0,1fr);min-height:100vh}

/* sidebar */
.sidebar{position:sticky;top:0;height:100vh;overflow:auto;overscroll-behavior:contain;padding:22px 18px 18px;background:var(--sidebar);border-right:1px solid var(--line);scrollbar-width:thin;scrollbar-color:var(--line) transparent;transition:background-color .18s,border-color .18s}
.sidebar::-webkit-scrollbar{width:6px}
.sidebar::-webkit-scrollbar-thumb{background:var(--line);border-radius:6px}
.sidebar-head{display:flex;align-items:center;gap:10px;margin-bottom:20px}
.brand{display:flex;align-items:center;gap:11px;color:var(--ink);text-decoration:none;flex:1;min-width:0}
.brand img{width:42px;height:42px;flex:0 0 42px}
.theme-toggle{display:inline-flex;align-items:center;justify-content:center;flex:0 0 auto;width:36px;height:36px;border-radius:6px;border:1px solid var(--line);background:var(--paper);color:var(--muted);cursor:pointer;padding:0;touch-action:manipulation;transition:border-color .15s,color .15s,background-color .15s,transform .12s}
.theme-toggle:hover{border-color:var(--coral);color:var(--coral)}
.theme-toggle:active{transform:scale(.94)}
.theme-toggle svg{width:17px;height:17px;display:block}
.theme-toggle .theme-icon-sun{display:none}
:root[data-theme="dark"] .theme-toggle .theme-icon-sun{display:block}
:root[data-theme="dark"] .theme-toggle .theme-icon-moon{display:none}
.brand strong{display:block;font-family:Fraunces,serif;font-size:1.32rem;line-height:1;letter-spacing:0}
.brand small{display:block;color:var(--muted);font-size:.74rem;margin-top:4px}
.search{display:block;margin:0 0 14px}
.search span{display:block;color:var(--muted);font-size:.72rem;font-weight:700;text-transform:uppercase;letter-spacing:0;margin-bottom:7px}
.search input{width:100%;border:1px solid var(--line);background:var(--paper);border-radius:6px;padding:10px 11px;font-size:.9rem;color:var(--ink);transition:border-color .15s,box-shadow .15s}
.search input:focus-visible{border-color:var(--focus);box-shadow:0 0 0 2px color-mix(in srgb,var(--focus) 20%,transparent);outline:0}
.sidebar-nav{border-bottom:1px solid var(--line)}
.nav-section{border-top:1px solid var(--line)}
.nav-section>summary{display:grid;grid-template-columns:minmax(0,1fr) auto 14px;align-items:center;gap:9px;min-height:44px;padding:4px 8px 4px 4px;list-style:none;color:var(--ink);cursor:pointer;touch-action:manipulation}
.nav-section>summary::-webkit-details-marker{display:none}
.nav-section>summary:hover{color:var(--coral)}
.nav-section>summary h2{margin:0;font-size:.77rem;font-weight:700;text-transform:uppercase;letter-spacing:0}
.nav-count{color:var(--muted);font-size:.72rem;font-variant-numeric:tabular-nums}
.nav-chevron{width:7px;height:7px;border-right:1.5px solid currentColor;border-bottom:1.5px solid currentColor;transform:rotate(45deg);transition:transform .15s}
.nav-section[open] .nav-chevron{transform:rotate(225deg)}
.nav-links{padding:0 0 10px}
.nav-link{display:block;color:var(--ink);text-decoration:none;border-radius:4px;padding:7px 10px;margin:1px 0;font-size:.9rem;line-height:1.35;border-left:2px solid transparent;overflow-wrap:anywhere;transition:background-color .12s,color .12s}
.nav-link:hover{background:color-mix(in srgb,var(--coral) 9%,transparent);color:var(--reef)}
.nav-link.active{background:var(--nav-active-bg);color:var(--nav-active-fg);border-left-color:var(--coral);font-weight:600}
.nav-empty{margin:16px 8px;color:var(--muted);font-size:.88rem}
.sidebar-foot{padding:16px 4px 0;font-size:.8rem}
.sidebar-foot a{color:var(--muted)}

/* main */
main{min-width:0;padding:30px 48px 80px;max-width:1360px;margin:0 auto;width:100%}
.hero{display:flex;align-items:flex-end;justify-content:space-between;gap:22px;border-bottom:1px solid var(--line);padding:16px 0 24px;position:relative;flex-wrap:wrap}
.hero:after{content:"";position:absolute;left:0;bottom:-1px;width:72px;height:3px;background:var(--coral)}
.hero-text{min-width:0;flex:1 1 320px}
.eyebrow{margin:0 0 8px;color:var(--coral);font-weight:700;text-transform:uppercase;letter-spacing:0;font-size:.76rem}
.hero h1{font-family:Fraunces,Georgia,serif;font-size:2.8rem;line-height:1.08;letter-spacing:0;margin:0;font-weight:700;color:var(--ink);text-wrap:balance}
.hero-meta{display:flex;gap:8px;flex:0 0 auto}
.repo,.edit{border:1px solid var(--line);color:var(--ink);text-decoration:none;border-radius:6px;padding:7px 12px;font-weight:600;font-size:.84rem;background:var(--paper);transition:border-color .15s,color .15s,background-color .15s}
.repo:hover,.edit:hover{border-color:var(--coral);color:var(--coral)}
.edit{color:var(--muted)}

/* landing hero */
.home main{max-width:1500px}
.hero-home{display:grid;grid-template-columns:minmax(0,1.08fr) minmax(380px,.92fr);align-items:center;gap:44px;padding:48px;border:1px solid var(--line);border-radius:24px;background:radial-gradient(circle at 6% 8%,color-mix(in srgb,var(--coral) 13%,transparent),transparent 28%),radial-gradient(circle at 93% 92%,color-mix(in srgb,var(--reef) 15%,transparent),transparent 31%),var(--paper);overflow:hidden}
.hero-home:after{display:none}
.home-hero-copy{min-width:0}
.home-title{display:flex;align-items:center;gap:12px;margin-bottom:34px}
.home-title img{width:56px;height:56px;flex:0 0 56px}
.home-title strong,.home-title small{display:block}
.home-title strong{font:700 1.42rem/1 Fraunces,Georgia,serif}
.home-title small{margin-top:5px;color:var(--muted);font-size:.68rem;font-weight:700;letter-spacing:.02em;text-transform:uppercase}
.hero-home h1{max-width:760px;font-size:clamp(3.45rem,5.7vw,6.2rem);line-height:.88;letter-spacing:-.045em;font-weight:700;margin:0;text-wrap:balance}
.hero-home h1 em{display:block;color:var(--coral);font-style:normal}
.lede{margin:24px 0 26px;color:var(--body-soft);font-size:1.08rem;line-height:1.58;max-width:62ch;text-wrap:pretty}
.cta{display:flex;gap:10px;flex-wrap:wrap}
.cta-primary,.cta-secondary{display:inline-flex;align-items:center;border-radius:6px;padding:10px 16px;font-weight:600;font-size:.93rem;text-decoration:none;touch-action:manipulation;transition:transform .15s,box-shadow .15s,background-color .15s,border-color .15s,color .15s}
.cta-primary{background:var(--ink);color:var(--paper);border:1px solid var(--ink)}
.cta-primary:hover{background:var(--reef);border-color:var(--reef);color:var(--paper);transform:translateY(-1px);box-shadow:0 8px 20px color-mix(in srgb,var(--reef) 22%,transparent)}
.cta-secondary{border:1px solid var(--ink);color:var(--ink);background:transparent}
.cta-secondary:hover{border-color:var(--coral);color:var(--coral);transform:translateY(-1px)}
.home-facts{display:flex;flex-wrap:wrap;gap:8px 22px;padding:0;margin:24px 0 0;color:var(--muted);font-size:.76rem;font-weight:700;list-style:none}
.home-facts li{position:relative;padding-left:14px}
.home-facts li:before{content:"";position:absolute;left:0;top:.58em;width:5px;height:5px;border-radius:50%;background:var(--reef)}
.home-console{min-width:0;padding:16px;border:1px solid var(--code-border);border-radius:18px;background:var(--code-bg);color:var(--code-fg);box-shadow:0 28px 72px rgba(0,0,0,.25)}
.home-console-bar{display:grid;grid-template-columns:1fr auto 1fr;align-items:center;padding:3px 3px 14px;border-bottom:1px solid rgba(255,255,255,.1);font-size:.72rem}
.home-console-bar span{color:var(--coral);letter-spacing:.12em}
.home-console-bar small{text-align:right;color:#7dd3c7}
.home-console pre{max-width:100%;margin:13px 0;padding:14px;overflow:auto;border:1px solid rgba(255,255,255,.09);border-radius:9px;background:#0d0f10;font:500 .76rem/1.55 "IBM Plex Mono",ui-monospace,monospace}
.home-console pre code{white-space:pre}
.home-console pre span{color:#efc15b}
.home-console ol{display:grid;gap:8px;padding:0;margin:0;list-style:none}
.home-console li{display:grid;grid-template-columns:42px minmax(0,1fr) 22px;align-items:center;gap:10px;padding:12px;border:1px solid rgba(255,255,255,.09);border-radius:10px;background:rgba(255,255,255,.035)}
.home-console li b{display:grid;place-items:center;width:32px;height:32px;border-radius:50%;background:rgba(255,255,255,.1);font:700 .66rem/1 "IBM Plex Mono",monospace}
.home-console li strong,.home-console li small,.home-console-result strong,.home-console-result small{display:block}
.home-console li small,.home-console-result small{overflow-wrap:anywhere;color:var(--code-comment)}
.home-console li i,.home-console-result>i{color:#7dd3c7;font-style:normal}
.home-console-result{display:flex;gap:11px;align-items:center;margin-top:10px;padding:13px;border-radius:10px;background:color-mix(in srgb,var(--reef) 30%,#111)}
.home-section{padding:82px 0;border-bottom:1px solid var(--line)}
.home-section>header{display:grid;grid-template-columns:minmax(0,1fr) minmax(300px,.72fr);align-items:end;gap:40px;margin-bottom:28px}
.home-section>header h2,.home-install h2{margin:0;color:var(--ink);font:700 clamp(2.3rem,4.1vw,4.4rem)/.98 Fraunces,Georgia,serif;letter-spacing:-.035em;text-wrap:balance}
.home-section>header>p{max-width:58ch;margin:0 0 5px;color:var(--body-soft);font-size:1rem;text-wrap:pretty}
.home-section>header code,.home-capability-note code{font-family:"IBM Plex Mono",ui-monospace,monospace;font-size:.86em}
.home-job-finder{padding:18px;border:1px solid var(--code-border);border-radius:22px;background:radial-gradient(circle at 100% 0,color-mix(in srgb,var(--reef) 34%,transparent),transparent 38%),var(--code-bg);color:var(--code-fg);box-shadow:0 24px 60px rgba(0,0,0,.16)}
.home-job-finder fieldset{min-width:0;padding:0;margin:0;border:0}
.home-job-layout{display:grid;grid-template-columns:minmax(300px,.82fr) minmax(0,1.18fr);gap:18px;align-items:stretch}
.home-job-choices{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px}
.home-job-option{position:relative;min-width:0}
.home-job-option label{display:flex;min-height:108px;height:100%;flex-direction:column;padding:15px;border:1px solid rgba(255,255,255,.12);border-radius:12px;background:rgba(255,255,255,.045);color:var(--code-fg);cursor:pointer;touch-action:manipulation;transition:transform .15s,border-color .15s,background-color .15s,box-shadow .15s}
.home-job-option label:hover{transform:translateY(-2px);border-color:rgba(125,211,199,.65);background:rgba(255,255,255,.075)}
.home-job-option input:focus-visible+label{outline:3px solid #7dd3c7;outline-offset:2px}
.home-job-option input:checked+label{border-color:#7dd3c7;background:color-mix(in srgb,var(--reef) 34%,#171a1c);box-shadow:inset 3px 0 0 #7dd3c7}
.home-job-option label>span{color:#7dd3c7;font:700 .64rem/1 "IBM Plex Mono",monospace}
.home-job-option label>strong{margin-top:12px;font:600 1.06rem/1.15 Fraunces,Georgia,serif;text-wrap:balance}
.home-job-option label>small{margin-top:auto;padding-top:8px;color:var(--code-comment);font-size:.69rem;line-height:1.35;overflow-wrap:anywhere}
.home-job-result{display:none;min-width:0;height:100%;grid-template-columns:minmax(0,.85fr) minmax(0,1.15fr);gap:20px;padding:26px;border:1px solid rgba(255,255,255,.13);border-radius:15px;background:linear-gradient(145deg,rgba(255,255,255,.075),rgba(255,255,255,.025))}
.home-job-result:first-child{display:grid}
.home-job-finder:has(.home-job-radio:checked) .home-job-result{display:none}
.home-job-finder:has(#home-job-fast-feedback:checked) [data-home-job-result="fast-feedback"],
.home-job-finder:has(#home-job-agent-sandbox:checked) [data-home-job-result="agent-sandbox"],
.home-job-finder:has(#home-job-cross-platform:checked) [data-home-job-result="cross-platform"],
.home-job-finder:has(#home-job-desktop:checked) [data-home-job-result="desktop"],
.home-job-finder:has(#home-job-fanout-testing:checked) [data-home-job-result="fanout-testing"],
.home-job-finder:has(#home-job-gpu:checked) [data-home-job-result="gpu"]{display:grid}
.home-job-result-copy{display:flex;min-width:0;flex-direction:column}
.home-job-result-copy .eyebrow{margin-bottom:12px}
.home-job-result h3{margin:0;color:var(--code-fg);font:600 2rem/1.05 Fraunces,Georgia,serif;text-wrap:balance}
.home-job-result-copy>p:not(.eyebrow){margin:16px 0;color:var(--code-comment);font-size:.9rem;text-wrap:pretty}
.home-job-command{display:flex;min-width:0;flex-direction:column;padding:14px;border:1px solid rgba(255,255,255,.1);border-radius:12px;background:#0d0f10}
.home-job-command>div{display:flex;justify-content:space-between;gap:12px;padding:1px 2px 12px;border-bottom:1px solid rgba(255,255,255,.09);font-size:.66rem}
.home-job-command>div span{color:#f0ebe2;font-weight:700}
.home-job-command>div small{color:#7dd3c7}
.home-job-command pre{position:relative;max-width:100%;margin:14px 0;padding:2px 58px 2px 0;overflow:auto;background:transparent;border:0;color:var(--code-fg);font:500 .72rem/1.75 "IBM Plex Mono",ui-monospace,monospace}
.home-job-command pre code{display:grid;min-width:0;gap:3px;white-space:normal}
.home-job-command-line{display:grid;min-width:0;grid-template-columns:auto minmax(0,1fr);gap:7px}
.home-job-command-line i{color:#efc15b;font-style:normal}
.home-job-command-line b{min-width:0;font:inherit;overflow-wrap:anywhere}
.home-job-command a{display:inline-flex;align-items:center;gap:8px;margin-top:auto;color:#7dd3c7;font-size:.75rem;font-weight:700;text-decoration:none}
.home-job-command a:hover{color:#ff8a78}
.home-job-command a i{font-style:normal;transition:transform .16s}
.home-job-command a:hover i{transform:translateX(3px)}
.home-job-disclaimer{margin:14px 3px 2px;color:var(--code-comment);font-size:.73rem;text-wrap:pretty}
.home-job-disclaimer strong{color:var(--code-fg)}
.home-section-link{display:inline-flex;align-items:center;gap:10px;margin-top:22px;color:var(--ink);font-weight:700;text-decoration:none}
.home-section-link span{transition:transform .16s}
.home-section-link:hover span{transform:translateX(3px)}
.home-path-grid{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));border:1px solid var(--line);border-radius:16px;background:var(--paper);overflow:hidden}
.home-path{display:flex;min-height:280px;flex-direction:column;padding:26px}
.home-path+.home-path{border-left:1px solid var(--line)}
.home-path>span{color:var(--coral);font:700 .7rem/1 "IBM Plex Mono",monospace}
.home-path>p{margin:38px 0 5px;color:var(--muted);font-size:.68rem;font-weight:700;text-transform:uppercase}
.home-path h3{margin:0 0 12px;font:600 1.55rem/1.1 Fraunces,Georgia,serif;text-wrap:balance}
.home-path>div{color:var(--body-soft);font-size:.91rem}
.home-path a{display:inline-flex;gap:8px;align-items:center;margin-top:auto;padding-top:25px;font-weight:700;text-decoration:none}
.home-path a i{font-style:normal;transition:transform .16s}
.home-path a:hover i{transform:translateX(3px)}
.home-price-grid{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:10px}
.home-price-grid article{min-width:0;padding:22px;border:1px solid var(--line);border-radius:12px;background:var(--paper)}
.home-price-grid span,.home-price-grid strong{display:block}
.home-price-grid span{color:var(--muted);font-size:.68rem;font-weight:700;text-transform:uppercase}
.home-price-grid strong{margin:24px 0 9px;font:600 1.25rem/1.12 Fraunces,Georgia,serif;overflow-wrap:anywhere}
.home-price-grid p{margin:0;color:var(--body-soft);font-size:.84rem;line-height:1.45}
.home-capability-note{display:grid;grid-template-columns:minmax(230px,.55fr) minmax(0,1.45fr);align-items:center;gap:38px;margin-top:28px;padding:26px 28px;border:1px solid color-mix(in srgb,var(--reef) 32%,var(--line));border-radius:14px;background:color-mix(in srgb,var(--reef) 7%,var(--paper))}
.home-capability-note .eyebrow{margin-bottom:5px}
.home-capability-note h2{margin:0;font:600 1.75rem/1.08 Fraunces,Georgia,serif;text-wrap:balance}
.home-capability-note>p{margin:0;color:var(--body-soft);text-wrap:pretty}
.home-capability-note a{font-weight:700;white-space:nowrap}
.home-install{display:grid;grid-template-columns:minmax(0,1fr) minmax(380px,.85fr);align-items:center;gap:48px;margin:82px 0 0;padding:38px 40px;border:1px solid var(--line);border-radius:18px;background:var(--paper)}
.home-install h2{font-size:clamp(2.2rem,3.6vw,3.6rem)}
.home-install>div>p:not(.eyebrow){max-width:58ch;margin:16px 0;color:var(--body-soft)}
.home-install a{display:inline-flex;gap:8px;align-items:center;font-weight:700;text-decoration:none}
.home-install pre{max-width:100%;margin:0;padding:20px;overflow:auto;border:1px solid var(--code-border);border-radius:12px;background:var(--code-bg);color:var(--code-fg);font:500 .78rem/1.8 "IBM Plex Mono",ui-monospace,monospace;box-shadow:0 14px 34px rgba(0,0,0,.18)}
.home-install pre code{white-space:pre}
.home-install pre span{color:#efc15b}
.home-trust{display:grid;grid-template-columns:minmax(260px,.65fr) minmax(0,1.35fr);gap:48px;align-items:start;margin-top:64px;padding:34px 0 0;border-top:1px solid var(--line)}
.home-trust h2{margin:0;font:600 1.7rem/1.1 Fraunces,Georgia,serif;text-wrap:balance}
.home-trust>p{max-width:72ch;margin:0;color:var(--body-soft)}

/* layout: doc + toc */
.doc-grid{display:grid;grid-template-columns:minmax(0,1fr);gap:36px;margin-top:30px}
@media(min-width:1180px){.doc-grid{grid-template-columns:minmax(0,78ch) 210px;justify-content:start}.doc-grid-wide{grid-template-columns:minmax(0,1fr)}}
.doc{min-width:0;max-width:78ch;overflow-wrap:break-word}
.doc-wide{max-width:none}
.doc h1{display:none}
.doc h2{font-family:Fraunces,Georgia,serif;font-size:1.65rem;line-height:1.15;margin:1.9em 0 .5em;font-weight:600;letter-spacing:0;position:relative;text-wrap:balance;scroll-margin-top:24px}
.doc h3{font-size:1.12rem;margin:1.6em 0 .3em;position:relative;font-weight:600}
.doc h4{font-size:.98rem;margin:1.3em 0 .2em;color:var(--reef);position:relative;font-weight:600}
.doc h2:first-child,.doc h3:first-child,.doc h4:first-child{margin-top:0}
.doc :is(h2,h3,h4) .anchor{position:absolute;left:-1em;top:0;color:var(--muted);opacity:0;text-decoration:none;font-weight:400;padding-right:.3em;transition:opacity .12s,color .12s}
.doc :is(h2,h3,h4):hover .anchor{opacity:.55}
.doc :is(h2,h3,h4) .anchor:hover{opacity:1;color:var(--coral)}
.doc p{margin:0 0 1.05em}
.doc ul,.doc ol{padding-left:1.35rem;margin:0 0 1.2em}
.doc li{margin:.25em 0}
.doc li>p{margin:0 0 .4em}
.doc strong{font-weight:600}
.doc code{font-family:"IBM Plex Mono",ui-monospace,monospace;font-size:.86em;background:var(--inline-bg);border:1px solid var(--inline-border);border-radius:5px;padding:.08em .34em}
.doc pre{position:relative;overflow:auto;background:var(--code-bg);color:var(--code-fg);border-radius:8px;padding:16px 20px;border:1px solid var(--code-border);box-shadow:inset 0 0 0 1px rgba(255,255,255,.03);margin:1.35em 0;font-size:.88em;scrollbar-width:thin;scrollbar-color:var(--code-scroll) transparent}
.doc pre::-webkit-scrollbar{height:8px}
.doc pre::-webkit-scrollbar-thumb{background:var(--code-scroll);border-radius:8px}
.doc pre code{display:block;background:transparent;border:0;color:inherit;padding:0;font-size:1em;white-space:pre;overflow-wrap:normal;word-break:normal;tab-size:2}
:is(.doc pre,[data-copyable]) .copy{position:absolute;top:8px;right:8px;background:rgba(255,251,244,.06);color:var(--code-fg);border:1px solid rgba(255,251,244,.18);border-radius:6px;padding:3px 9px;font:600 .7rem/1 "IBM Plex Sans",sans-serif;cursor:pointer;opacity:0;transition:opacity .15s,background-color .15s,border-color .15s}
:is(.doc pre,[data-copyable]):hover .copy,:is(.doc pre,[data-copyable]) .copy:focus-visible{opacity:1}
:is(.doc pre,[data-copyable]) .copy:hover{background:rgba(255,251,244,.14)}
:is(.doc pre,[data-copyable]) .copy.copied{background:var(--coral);border-color:var(--coral);opacity:1}
.home-job-command [data-copyable] .copy{min-width:44px;min-height:44px;opacity:1}
.doc blockquote{margin:1.4em 0;padding:12px 16px;border-left:3px solid var(--coral);background:var(--quote-bg);border-radius:0 8px 8px 0;color:var(--ink)}
.doc blockquote p:last-child{margin-bottom:0}
.table-scroll{width:100%;max-width:100%;overflow-x:auto;overscroll-behavior-inline:contain;margin:1.2em 0;border-top:1px solid var(--line);border-bottom:1px solid var(--line);scrollbar-width:thin;scrollbar-color:var(--code-scroll) transparent}
.table-scroll:focus-visible{outline:3px solid var(--focus);outline-offset:3px}
.doc table{width:100%;min-width:680px;border-collapse:collapse;font-size:.92em;font-variant-numeric:tabular-nums}
.doc th,.doc td{border-bottom:1px solid var(--line);padding:10px 12px;text-align:left;vertical-align:top}
.doc th{font-weight:600;color:var(--reef)}
.doc td code{overflow-wrap:anywhere;word-break:break-word}
.doc tbody tr:last-child td{border-bottom:0}
.doc tbody tr:hover td{background:color-mix(in srgb,var(--reef) 5%,var(--paper))}
.provider-filter{margin:1.4em 0 1em;padding:18px 0;border-top:1px solid var(--line);border-bottom:1px solid var(--line)}
.provider-filter-head{display:flex;align-items:baseline;justify-content:space-between;gap:18px;margin-bottom:8px}
.provider-filter label{font-weight:700;color:var(--ink)}
.provider-filter output{color:var(--muted);font-size:.86rem;font-variant-numeric:tabular-nums}
.provider-filter>input{width:100%;border:1px solid var(--line);background:var(--paper);color:var(--ink);border-radius:6px;padding:11px 12px}
.provider-filter-groups{display:flex;gap:0;margin-top:10px;overflow-x:auto;overscroll-behavior-inline:contain;padding:2px 0 5px;scrollbar-width:thin}
.provider-filter-groups button{flex:0 0 auto;border:1px solid var(--line);border-right:0;background:var(--paper);color:var(--muted);padding:7px 10px;font-size:.78rem;font-weight:600;cursor:pointer;touch-action:manipulation}
.provider-filter-groups button:first-child{border-radius:5px 0 0 5px}
.provider-filter-groups button:last-child{border-right:1px solid var(--line);border-radius:0 5px 5px 0}
.provider-filter-groups button:hover{color:var(--ink);background:var(--inline-bg)}
.provider-filter-groups button[aria-pressed="true"]{background:var(--ink);border-color:var(--ink);color:var(--paper)}
.provider-filter-groups button[aria-pressed="true"]+button{border-left-color:var(--ink)}
.provider-empty{margin:14px 0 0;color:var(--muted)}
.provider-matrix{min-width:1180px!important}
.provider-matrix :is(th,td):first-child{position:sticky;left:0;z-index:1;background:var(--paper);box-shadow:1px 0 0 var(--line)}
.provider-matrix th:first-child{z-index:2}
.provider-matrix tbody tr:hover td:first-child{background:color-mix(in srgb,var(--reef) 5%,var(--paper))}
.doc hr{border:0;border-top:1px solid var(--line);margin:2em 0}

/* toc */
.toc{position:sticky;top:24px;align-self:start;font-size:.85rem;padding-left:14px;border-left:1px solid var(--line);max-height:calc(100vh - 48px);overflow:auto;scrollbar-width:thin;scrollbar-color:var(--line) transparent}
.toc::-webkit-scrollbar{width:5px}
.toc::-webkit-scrollbar-thumb{background:var(--line);border-radius:5px}
.toc h2{font-size:.7rem;color:var(--muted);text-transform:uppercase;letter-spacing:0;margin:0 0 10px;font-weight:700}
.toc a{display:block;color:var(--muted);text-decoration:none;padding:4px 0 4px 10px;line-height:1.35;border-left:2px solid transparent;margin-left:-12px;transition:color .12s,border-color .12s}
.toc a:hover{color:var(--ink)}
.toc a.active{color:var(--reef);border-left-color:var(--coral);font-weight:600}
.toc-l3{padding-left:22px!important;font-size:.94em}
@media(max-width:1179px){.toc{display:none}}

/* prev/next pager */
.page-nav{display:grid;grid-template-columns:1fr 1fr;gap:14px;margin-top:48px}
.page-nav>a{display:block;border:1px solid var(--line);background:var(--paper);border-radius:8px;padding:14px 18px;text-decoration:none;color:var(--ink);transition:border-color .15s,transform .15s,box-shadow .15s}
.page-nav>a:hover{border-color:var(--coral);transform:translateY(-1px);box-shadow:0 6px 18px rgba(0,0,0,.08)}
.page-nav small{display:block;color:var(--muted);font-size:.7rem;text-transform:uppercase;letter-spacing:0;margin-bottom:5px;font-weight:700}
.page-nav span{display:block;font-weight:600;line-height:1.3}
.page-nav-prev{text-align:left}
.page-nav-next{text-align:right;grid-column:2}
.page-nav-prev:only-child{grid-column:1}

/* mobile nav toggle */
.nav-toggle{display:none;position:fixed;top:14px;right:14px;top:calc(14px + env(safe-area-inset-top,0px));right:calc(14px + env(safe-area-inset-right,0px));z-index:30;width:42px;height:42px;border-radius:6px;background:var(--paper);border:1px solid var(--line);color:var(--ink);cursor:pointer;padding:10px 9px;flex-direction:column;align-items:stretch;justify-content:space-between;box-shadow:0 6px 18px rgba(0,0,0,.12);touch-action:manipulation}
.nav-toggle span{display:block;width:100%;height:2px;flex:0 0 2px;background:currentColor;border-radius:2px;transition:transform .2s,opacity .2s}
.nav-toggle[aria-expanded="true"] span:nth-child(1){transform:translateY(8px) rotate(45deg)}
.nav-toggle[aria-expanded="true"] span:nth-child(2){opacity:0}
.nav-toggle[aria-expanded="true"] span:nth-child(3){transform:translateY(-8px) rotate(-45deg)}
.nav-backdrop{display:none;position:fixed;inset:0;z-index:20;width:100%;height:100%;border:0;background:rgba(0,0,0,.48);padding:0;cursor:pointer}

/* mobile */
@media(max-width:1280px){
  .hero-home{grid-template-columns:1fr;padding:40px}
  .home-console{max-width:760px}
  .home-price-grid{grid-template-columns:repeat(2,minmax(0,1fr))}
}
@media(max-width:900px){
  .shell{display:block}
  body.nav-open{overflow:hidden}
  .sidebar{position:fixed;inset:0 auto 0 0;width:min(86vw,340px);max-width:none;height:100vh;height:100dvh;z-index:25;transform:translateX(-100%);transition:transform .25s ease;box-shadow:0 18px 40px rgba(0,0,0,.24);background:var(--sidebar);pointer-events:none;padding-top:calc(20px + env(safe-area-inset-top,0px));padding-bottom:calc(18px + env(safe-area-inset-bottom,0px));padding-left:calc(18px + env(safe-area-inset-left,0px))}
  .sidebar.open{transform:translateX(0);pointer-events:auto}
  .nav-backdrop:not([hidden]){display:block}
  .nav-toggle{display:flex}
  main{padding:68px 20px 56px}
  .hero{padding-top:8px}
  .hero h1{font-size:2.2rem}
  .hero-meta{width:100%;justify-content:flex-start}
  .hero-home{padding:32px}
  .hero-home h1{font-size:clamp(3.2rem,10vw,5rem)}
  .home-section{padding:64px 0}
  .home-section>header{grid-template-columns:1fr;gap:16px}
  .home-job-layout{grid-template-columns:1fr}
  .home-job-choices{grid-template-columns:repeat(3,minmax(0,1fr))}
  .home-path-grid{grid-template-columns:1fr}
  .home-path{min-height:230px}
  .home-path+.home-path{border-top:1px solid var(--line);border-left:0}
  .home-capability-note{grid-template-columns:1fr;gap:12px}
  .home-install{grid-template-columns:1fr;margin-top:64px}
  .home-trust{grid-template-columns:1fr;gap:16px}
  .doc-grid{margin-top:22px;gap:24px}
  .doc :is(h2,h3,h4) .anchor{display:none}
}
@media(max-width:520px){
  main{padding:64px 16px 48px}
  .hero-home{padding:24px 18px;border-radius:18px}
  .home-title{gap:10px;margin-bottom:28px}
  .home-title img{width:48px;height:48px;flex-basis:48px}
  .home-title strong{font-size:1.24rem}
  .home-title small{font-size:.58rem}
  .hero-home h1{font-size:clamp(2.85rem,15vw,4rem)}
  .lede{font-size:1rem}
  .cta-primary,.cta-secondary{width:100%;justify-content:center}
  .home-facts{display:grid;gap:7px}
  .home-console{margin-top:4px;padding:11px;border-radius:13px}
  .home-console-bar{grid-template-columns:1fr auto}
  .home-console-bar strong{display:none}
  .home-console pre{font-size:.68rem}
  .home-console li{grid-template-columns:36px minmax(0,1fr) 18px;padding:10px}
  .home-console li b{width:29px;height:29px}
  .home-price-grid{grid-template-columns:1fr}
  .home-section{padding:54px 0}
  .home-section>header h2,.home-install h2{font-size:2.45rem}
  .home-job-finder{margin:0 -2px;padding:10px;border-radius:16px}
  .home-job-choices{grid-template-columns:repeat(2,minmax(0,1fr))}
  .home-job-option label{min-height:76px;padding:11px}
  .home-job-option label>strong{font-size:.98rem}
  .home-job-option label>small{display:none}
  .home-job-result{grid-template-columns:1fr;gap:18px;padding:19px}
  .home-job-result h3{font-size:1.8rem}
  .home-job-command pre{font-size:.66rem}
  .home-capability-note{padding:22px 20px}
  .home-capability-note a{white-space:normal}
  .home-install{gap:28px;margin-top:54px;padding:28px 20px}
  .home-install pre{margin:0 -6px;padding:16px;font-size:.7rem}
  .page-nav{grid-template-columns:1fr}
  .page-nav-next{grid-column:1}
  .doc pre{margin-left:-16px;margin-right:-16px;border-radius:0;border-left:0;border-right:0}
}
@media(prefers-reduced-motion:reduce){html{scroll-behavior:auto}*,*:before,*:after{transition-duration:.01ms!important;animation-duration:.01ms!important;animation-iteration-count:1!important}}
`;
}

function js() {
  return `
const themeRoot=document.documentElement;
function storedDocsTheme(){try{return localStorage.getItem('crabbox-docs-theme')}catch(e){return null}}
function applyDocsTheme(mode){
  themeRoot.dataset.theme=mode;
  const themeColor=document.getElementById('theme-color');
  if(themeColor)themeColor.content=mode==='dark'?'#17191b':'#f4f5f5';
  document.querySelectorAll('[data-theme-toggle]').forEach((button)=>{
    button.setAttribute('aria-pressed',mode==='dark'?'true':'false');
    button.setAttribute('aria-label',mode==='dark'?'Switch to light mode':'Switch to dark mode');
  });
}
applyDocsTheme(themeRoot.dataset.theme==='dark'?'dark':'light');
document.querySelectorAll('[data-theme-toggle]').forEach((btn)=>{
  btn.addEventListener('click',()=>{
    const next=themeRoot.dataset.theme==='dark'?'light':'dark';
    applyDocsTheme(next);
    try{localStorage.setItem('crabbox-docs-theme',next)}catch(e){}
  });
});
const docsSystemDark=window.matchMedia&&matchMedia('(prefers-color-scheme: dark)');
if(docsSystemDark){
  const onDocsSystemChange=(e)=>{if(storedDocsTheme())return;applyDocsTheme(e.matches?'dark':'light')};
  if(docsSystemDark.addEventListener)docsSystemDark.addEventListener('change',onDocsSystemChange);
  else docsSystemDark.addListener?.(onDocsSystemChange);
}

const sidebar=document.querySelector('.sidebar');
const toggle=document.querySelector('.nav-toggle');
const backdrop=document.querySelector('.nav-backdrop');
const main=document.querySelector('main');
const mobileNav=window.matchMedia('(max-width: 900px)');
const sidebarFocusable='a[href],button:not([disabled]),input:not([disabled]),summary,[tabindex]:not([tabindex="-1"])';
function setSidebarOpen(open,{focus=false,restore=false}={}){
  if(!sidebar||!toggle)return;
  const mobile=mobileNav.matches;
  open=mobile&&open;
  sidebar.classList.toggle('open',open);
  toggle.setAttribute('aria-expanded',open?'true':'false');
  toggle.setAttribute('aria-label',open?'Close navigation':'Open navigation');
  document.body.classList.toggle('nav-open',open);
  if(backdrop){backdrop.hidden=!open;backdrop.setAttribute('aria-hidden',open?'false':'true')}
  if(mobile){
    sidebar.inert=!open;
    if(open)sidebar.removeAttribute('aria-hidden');
    else sidebar.setAttribute('aria-hidden','true');
    if(main)main.inert=open;
  }else{
    sidebar.inert=false;
    sidebar.removeAttribute('aria-hidden');
    if(main)main.inert=false;
  }
  if(open&&focus)sidebar.focus({preventScroll:true});
  if(!open&&restore)toggle.focus({preventScroll:true});
}
setSidebarOpen(false);
toggle?.addEventListener('click',()=>setSidebarOpen(!sidebar?.classList.contains('open'),{focus:true,restore:true}));
backdrop?.addEventListener('click',()=>setSidebarOpen(false,{restore:true}));
document.addEventListener('keydown',(e)=>{if(e.key==='Escape'&&sidebar?.classList.contains('open'))setSidebarOpen(false,{restore:true})});
sidebar?.addEventListener('keydown',(e)=>{
  if(e.key!=='Tab'||!mobileNav.matches||!sidebar.classList.contains('open'))return;
  const focusable=[...sidebar.querySelectorAll(sidebarFocusable)].filter((el)=>el.getClientRects().length);
  if(!focusable.length)return;
  const first=focusable[0];
  const last=focusable[focusable.length-1];
  if(e.shiftKey&&(document.activeElement===first||document.activeElement===sidebar)){e.preventDefault();last.focus()}
  else if(!e.shiftKey&&document.activeElement===last){e.preventDefault();first.focus()}
});
const syncSidebarForViewport=()=>setSidebarOpen(false);
if(mobileNav.addEventListener)mobileNav.addEventListener('change',syncSidebarForViewport);
else mobileNav.addListener?.(syncSidebarForViewport);
const activeNavLink=sidebar?.querySelector('.nav-link.active');
if(sidebar&&activeNavLink)requestAnimationFrame(()=>{sidebar.scrollTop=Math.max(0,activeNavLink.offsetTop-sidebar.clientHeight/2)});

const input=document.getElementById('doc-search');
const navSections=[...document.querySelectorAll('[data-nav-section]')];
const navEmpty=document.querySelector('.nav-empty');
let navOpenState=null;
const navParams=new URLSearchParams(location.search);
if(input)input.value=navParams.get('docs')||'';
const syncNavURL=()=>{
  if(location.protocol==='file:')return;
  const next=new URL(location.href);
  const value=input?.value.trim()||'';
  value?next.searchParams.set('docs',value):next.searchParams.delete('docs');
  history.replaceState(null,'',next);
};
const applyNavFilters=(updateURL=true)=>{
  const q=input.value.trim().toLowerCase();
  const terms=q.split(/\\s+/).filter(Boolean);
  if(q&&!navOpenState)navOpenState=new Map(navSections.map((section)=>[section,section.open]));
  let matches=0;
  navSections.forEach((section)=>{
    let sectionMatches=0;
    section.querySelectorAll('.nav-link').forEach((link)=>{
      const haystack=link.dataset.navSearch||link.textContent.toLowerCase();
      const match=!q||terms.every((term)=>haystack.includes(term));
      link.hidden=!match;
      if(match)sectionMatches+=1;
    });
    section.hidden=Boolean(q&&!sectionMatches);
    if(q)section.open=sectionMatches>0;
    else if(navOpenState?.has(section))section.open=navOpenState.get(section);
    matches+=sectionMatches;
  });
  if(!q)navOpenState=null;
  if(navEmpty)navEmpty.hidden=!q||matches>0;
  if(updateURL)syncNavURL();
};
input?.addEventListener('input',()=>applyNavFilters());
if(input?.value)applyNavFilters(false);

const providerFilter=document.querySelector('[data-provider-filter]');
if(providerFilter){
  const providerInput=providerFilter.querySelector('input');
  const providerButtons=[...providerFilter.querySelectorAll('[data-provider-group-filter]')];
  const providerRows=[...document.querySelectorAll('.provider-matrix tbody tr')];
  const providerCount=providerFilter.querySelector('[data-provider-count]');
  const providerEmpty=providerFilter.querySelector('[data-provider-empty]');
  const providerParams=new URLSearchParams(location.search);
  let providerGroup=providerParams.get('group')||'all';
  if(!providerButtons.some((button)=>button.dataset.providerGroupFilter===providerGroup))providerGroup='all';
  if(providerInput)providerInput.value=providerParams.get('q')||'';
  providerButtons.forEach((button)=>button.setAttribute('aria-pressed',button.dataset.providerGroupFilter===providerGroup?'true':'false'));
  const syncProviderURL=()=>{
    if(location.protocol==='file:')return;
    const next=new URL(location.href);
    const value=providerInput?.value.trim()||'';
    value?next.searchParams.set('q',value):next.searchParams.delete('q');
    providerGroup!=='all'?next.searchParams.set('group',providerGroup):next.searchParams.delete('group');
    history.replaceState(null,'',next);
  };
  const applyProviderFilters=(updateURL=true)=>{
    const terms=(providerInput?.value.trim().toLowerCase()||'').split(/\\s+/).filter(Boolean);
    let count=0;
    providerRows.forEach((row)=>{
      const groups=(row.dataset.providerGroups||'').split(/\\s+/);
      const groupMatch=providerGroup==='all'||groups.includes(providerGroup);
      const search=row.dataset.providerSearch||row.textContent.toLowerCase();
      const textMatch=terms.every((term)=>search.includes(term));
      const match=groupMatch&&textMatch;
      row.hidden=!match;
      if(match)count+=1;
    });
    if(providerCount)providerCount.textContent=count===1?'1 provider':count+' providers';
    if(providerEmpty)providerEmpty.hidden=count>0;
    if(updateURL)syncProviderURL();
  };
  providerInput?.addEventListener('input',applyProviderFilters);
  providerButtons.forEach((button)=>button.addEventListener('click',()=>{
    providerGroup=button.dataset.providerGroupFilter;
    providerButtons.forEach((item)=>item.setAttribute('aria-pressed',item===button?'true':'false'));
    applyProviderFilters();
  }));
  applyProviderFilters(false);
}

const homeJobFinder=document.querySelector('[data-home-job-finder]');
if(homeJobFinder){
  const homeJobRadios=[...homeJobFinder.querySelectorAll('[data-home-job-radio]')];
  const selectHomeJob=(value)=>{
    const radio=homeJobRadios.find((item)=>item.value===value)||homeJobRadios[0];
    if(radio)radio.checked=true;
    return radio?.value||'';
  };
  const homeJobParams=new URLSearchParams(location.search);
  const syncHomeJobURL=(value)=>{
    if(location.protocol==='file:')return;
    const next=new URL(location.href);
    if(value)next.searchParams.set('job',value);
    else next.searchParams.delete('job');
    history.replaceState(null,'',next);
  };
  const requestedHomeJob=homeJobParams.get('job');
  const selectedHomeJob=selectHomeJob(requestedHomeJob);
  if(requestedHomeJob&&requestedHomeJob!==selectedHomeJob)syncHomeJobURL('');
  homeJobRadios.forEach((radio)=>radio.addEventListener('change',()=>{
    if(radio.checked)syncHomeJobURL(radio.value);
  }));
  window.addEventListener('popstate',()=>{
    const requested=new URLSearchParams(location.search).get('job');
    const selected=selectHomeJob(requested);
    if(requested&&requested!==selected)syncHomeJobURL('');
  });
}

document.querySelectorAll('.doc pre,[data-copyable]').forEach(pre=>{const btn=document.createElement('button');const copyLabel=pre.dataset.copyLabel||'Copy code';btn.type='button';btn.className='copy';btn.setAttribute('aria-live','polite');btn.setAttribute('aria-label',copyLabel);btn.textContent='Copy';btn.addEventListener('click',async()=>{const code=pre.dataset.copyText??pre.querySelector('code')?.textContent??'';try{await navigator.clipboard.writeText(code);btn.textContent='Copied';btn.setAttribute('aria-label','Copied');btn.classList.add('copied');setTimeout(()=>{btn.textContent='Copy';btn.setAttribute('aria-label',copyLabel);btn.classList.remove('copied')},1400)}catch{btn.textContent='Failed';btn.setAttribute('aria-label','Copy failed');setTimeout(()=>{btn.textContent='Copy';btn.setAttribute('aria-label',copyLabel)},1400)}});pre.appendChild(btn)});

const tocLinks=document.querySelectorAll('.toc a');
if(tocLinks.length){const map=new Map();tocLinks.forEach(a=>{const id=a.getAttribute('href').slice(1);const el=document.getElementById(id);if(el)map.set(el,a)});const setActive=l=>{tocLinks.forEach(x=>x.classList.remove('active'));l.classList.add('active')};const obs=new IntersectionObserver(entries=>{const visible=entries.filter(e=>e.isIntersecting).sort((a,b)=>a.boundingClientRect.top-b.boundingClientRect.top);if(visible.length){const link=map.get(visible[0].target);if(link)setActive(link)}},{rootMargin:'-15% 0px -65% 0px',threshold:0});map.forEach((_,el)=>obs.observe(el))}
`;
}

function crabSvg() {
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 120" role="img" aria-label="Crabbox">
<rect width="120" height="120" rx="24" fill="#12211f"/>
<path d="M24 60c9-26 62-26 72 0 3 9-4 28-36 28S21 69 24 60Z" fill="#e35e46"/>
<path d="M38 55c4-8 12-13 22-13s18 5 22 13" fill="none" stroke="#fffbf4" stroke-width="6" stroke-linecap="round"/>
<circle cx="48" cy="62" r="5" fill="#12211f"/><circle cx="72" cy="62" r="5" fill="#12211f"/>
<path d="M27 54 11 42m82 12 16-12M36 82 22 96m62-14 14 14M46 86l-5 17m33-17 5 17" stroke="#fffbf4" stroke-width="7" stroke-linecap="round"/>
<path d="M20 35c-4-13 8-22 18-14-10 2-13 9-18 14Zm80 0c4-13-8-22-18-14 10 2 13 9 18 14Z" fill="#e35e46"/>
</svg>`;
}

function slug(text) {
  let out = "";
  let lastDash = false;
  for (const char of text.toLowerCase()) {
    if (char === "`") continue;
    const code = char.charCodeAt(0);
    const ok = (code >= 97 && code <= 122) || (code >= 48 && code <= 57);
    if (ok) {
      out += char;
      lastDash = false;
    } else if (!lastDash) {
      out += "-";
      lastDash = true;
    }
  }
  return trimDashes(out);
}

function firstIndex(left, right) {
  if (left < 0) return right;
  if (right < 0) return left;
  return Math.min(left, right);
}

function stripHeadingAnchor(value) {
  if (!value.startsWith('<a class="anchor"')) return value;
  const end = value.indexOf("</a>");
  return end >= 0 ? value.slice(end + "</a>".length) : value;
}

function stripHtmlTags(value) {
  let out = "";
  let inTag = false;
  for (const char of value) {
    if (char === "<") {
      inTag = true;
      continue;
    }
    if (char === ">") {
      inTag = false;
      continue;
    }
    if (!inTag) out += char;
  }
  return out;
}

function trimDashes(value) {
  let start = 0;
  let end = value.length;
  while (start < end && value[start] === "-") start += 1;
  while (end > start && value[end - 1] === "-") end -= 1;
  return value.slice(start, end);
}

function escapeHtml(value) {
  return String(value).replace(/[&<>"']/g, (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[char]);
}

function escapeAttr(value) {
  return escapeHtml(value);
}
