#!/usr/bin/env node
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

const sections = [
  ["Start", ["README.md", "getting-started.md", "how-it-works.md", "architecture.md", "orchestrator.md", "cli.md"]],
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
console.log(`built docs site: ${path.relative(root, outDir)}`);

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
    .filter((name) => name.endsWith(".md"))
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
  const prevNext = !isHome && (prev || next) ? pageNavHtml(prev, next, rootPrefix) : "";
  const heroBlock = isHome ? landingHero(rootPrefix) : standardHero(page, sectionName, editUrl);
  const articleClass = isHome ? "doc doc-home" : isProviderIndex ? "doc doc-wide" : "doc";
  const tocBlock = isHome || isProviderIndex ? "" : toc;
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <meta name="theme-color" id="theme-color" content="#f4f5f5">
  <title>${escapeHtml(page.title)} - Crabbox Docs</title>
  <link rel="icon" href="${rootPrefix}crabbox.svg">
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
          <span><strong>Crabbox</strong><small>Remote testbox docs</small></span>
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
      <div class="doc-grid${isHome ? " doc-grid-home" : ""}${isProviderIndex ? " doc-grid-wide" : ""}">
        <article class="${articleClass}">${html}${prevNext}</article>
        ${tocBlock}
      </div>
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
  const features = [
    ["Local loop, remote box", "Keep your editor and git workflow. Crabbox rsyncs your dirty checkout to a leased remote box and streams the run back."],
    ["Brokered, not BYO creds", "A Cloudflare Worker holds provider credentials and serializes lease state. Your CLI only carries a bearer token."],
    ["Cost-aware leases", "TTL-bounded machines, monthly spend caps, and per-user / per-org / per-provider usage from the broker."],
    ["Reuse what's warm", "<code>crabbox warmup</code> keeps a box hot. Reuse it with <code>--id</code> across runs, SSH, and CI hydration."],
    ["Many providers, one loop", "Brokered Hetzner / AWS / Azure, delegated E2B / Daytona / Blacksmith / Semaphore, or static SSH targets - Linux, Windows, and macOS."],
    ["Plays with Actions", "<code>actions hydrate</code> reuses your repository's GitHub Actions setup steps so local runs land in the same hydrated workspace."],
    ["Desktop in your browser", "<code>crabbox webvnc</code> streams a Linux, macOS, or Windows desktop into the browser. Drive UI tests, capture screenshots, hand the live session to a teammate - no VPN."],
    ["Proof for every run", "<code>crabbox artifacts collect</code> bundles screenshots, video, JUnit summaries, logs, and lease metadata. Drop it on a PR as before/after evidence instead of scraping log output."],
  ];
  const cards = features
    .map(([title, body]) => `<article class="feature"><h3>${escapeHtml(title)}</h3><p>${body}</p></article>`)
    .join("");
  return `<header class="hero hero-home">
        <div class="home-title">
          <img src="${rootPrefix}crabbox.svg" alt="" width="72" height="72">
          <div><p class="eyebrow">Remote execution control plane</p><h1>Crabbox</h1></div>
        </div>
        <p class="home-tagline">A short-lived box for every run.</p>
        <p class="lede">Keep the local edit-and-run loop while Crabbox leases, syncs, executes, and releases work across shared cloud capacity.</p>
        <div class="cta">
          <a class="cta-primary" href="${rootPrefix}getting-started.html">Get started</a>
          <a class="cta-secondary" href="${rootPrefix}providers/index.html">Browse ${Object.keys(providerMetadata).length} providers</a>
        </div>
        <pre class="hero-snippet" aria-label="Example Crabbox run"><code><span class="prompt">$</span> crabbox run -- pnpm test
<span class="comment"># lease cbx_8f2 - hetzner cax21 - ready 11s</span>
<span class="comment"># sync 184 files (1.2 MB)</span>
<span class="comment"># tests passed in 47s - released</span></code></pre>
      </header>
      <section class="features" aria-labelledby="highlights-heading"><h2 class="sr-only" id="highlights-heading">Highlights</h2>${cards}</section>`;
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
body{margin:0;background:var(--shell);color:var(--ink);font-family:"IBM Plex Sans",Avenir Next,sans-serif;line-height:1.65;overflow-x:hidden;-webkit-font-smoothing:antialiased;transition:background-color .18s,color .18s}
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
.hero-home{display:block;border-bottom:1px solid var(--line);padding:42px 0 34px}
.hero-home:after{display:none}
.home-title{display:flex;align-items:center;gap:18px;margin-bottom:22px}
.home-title img{width:72px;height:72px;flex:0 0 72px}
.home-title .eyebrow{margin-bottom:4px}
.hero-home h1{font-size:4rem;line-height:.95;letter-spacing:0;font-weight:700;margin:0}
.home-tagline{font-family:Fraunces,Georgia,serif;font-size:1.7rem;line-height:1.2;margin:0 0 10px;color:var(--ink);text-wrap:balance}
.lede{margin:0 0 22px;color:var(--body-soft);font-size:1.08rem;line-height:1.55;max-width:62ch;text-wrap:pretty}
.cta{display:flex;gap:10px;flex-wrap:wrap}
.cta-primary,.cta-secondary{display:inline-flex;align-items:center;border-radius:6px;padding:10px 16px;font-weight:600;font-size:.93rem;text-decoration:none;touch-action:manipulation;transition:transform .15s,box-shadow .15s,background-color .15s,border-color .15s,color .15s}
.cta-primary{background:var(--ink);color:var(--paper);border:1px solid var(--ink)}
.cta-primary:hover{background:var(--reef);border-color:var(--reef);color:var(--paper);transform:translateY(-1px);box-shadow:0 8px 20px color-mix(in srgb,var(--reef) 22%,transparent)}
.cta-secondary{border:1px solid var(--ink);color:var(--ink);background:transparent}
.cta-secondary:hover{border-color:var(--coral);color:var(--coral);transform:translateY(-1px)}
.hero-snippet{max-width:880px;margin:30px 0 0;background:var(--code-bg);color:var(--code-fg);border-radius:8px;padding:20px 22px;font:500 .88rem/1.65 "IBM Plex Mono",ui-monospace,monospace;border:1px solid var(--code-border);box-shadow:0 16px 34px rgba(0,0,0,.18);overflow:auto}
.hero-snippet code{background:transparent;border:0;padding:0;color:inherit;font:inherit;display:block;white-space:pre}
.hero-snippet .prompt{color:var(--ochre)}
.hero-snippet .comment{color:var(--code-comment)}

/* feature grid */
.features{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px;margin:26px 0 8px}
.feature{background:var(--panel);border:1px solid var(--line-soft);border-radius:8px;padding:18px 18px 16px;transition:border-color .15s,transform .15s,box-shadow .15s}
.feature:hover{border-color:var(--coral);transform:translateY(-2px);box-shadow:0 10px 24px rgba(0,0,0,.08)}
.feature h3{font-family:Fraunces,Georgia,serif;font-size:1.05rem;margin:0 0 6px;font-weight:600;letter-spacing:0;line-height:1.2}
.feature p{margin:0;color:var(--body-soft);font-size:.92rem;line-height:1.5}
.feature code{font-size:.86em;background:var(--inline-bg);border:1px solid var(--inline-border);border-radius:5px;padding:.04em .3em}

/* layout: doc + toc */
.doc-grid{display:grid;grid-template-columns:minmax(0,1fr);gap:36px;margin-top:30px}
.doc-grid-home{margin-top:18px}
.doc-home{padding:8px 0 0;max-width:78ch;margin-inline:auto;width:100%}
.doc-home>:first-child{margin-top:0}
@media(min-width:1180px){.doc-grid{grid-template-columns:minmax(0,78ch) 210px;justify-content:start}.doc-grid-home,.doc-grid-wide{grid-template-columns:minmax(0,1fr)}}
.doc{min-width:0;max-width:78ch;overflow-wrap:break-word}
.doc-home{max-width:none}
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
.doc pre .copy{position:absolute;top:8px;right:8px;background:rgba(255,251,244,.06);color:var(--code-fg);border:1px solid rgba(255,251,244,.18);border-radius:6px;padding:3px 9px;font:600 .7rem/1 "IBM Plex Sans",sans-serif;cursor:pointer;opacity:0;transition:opacity .15s,background .15s,border-color .15s}
.doc pre:hover .copy,.doc pre .copy:focus{opacity:1}
.doc pre .copy:hover{background:rgba(255,251,244,.14)}
.doc pre .copy.copied{background:var(--coral);border-color:var(--coral);opacity:1}
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
  .hero-home{padding-top:18px}
  .hero-home h1{font-size:3rem}
  .home-tagline{font-size:1.45rem}
  .hero-snippet{font-size:.78rem;padding:16px 16px}
  .features{grid-template-columns:repeat(2,minmax(0,1fr));margin-top:22px}
  .doc-grid{margin-top:22px;gap:24px}
  .doc :is(h2,h3,h4) .anchor{display:none}
}
@media(max-width:520px){
  main{padding:64px 16px 48px}
  .home-title{gap:13px}
  .home-title img{width:58px;height:58px;flex-basis:58px}
  .hero-home h1{font-size:2.65rem}
  .features{grid-template-columns:1fr}
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
input?.addEventListener('input',()=>{
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
});

const providerFilter=document.querySelector('[data-provider-filter]');
if(providerFilter){
  const providerInput=providerFilter.querySelector('input');
  const providerButtons=[...providerFilter.querySelectorAll('[data-provider-group-filter]')];
  const providerRows=[...document.querySelectorAll('.provider-matrix tbody tr')];
  const providerCount=providerFilter.querySelector('[data-provider-count]');
  const providerEmpty=providerFilter.querySelector('[data-provider-empty]');
  let providerGroup='all';
  const applyProviderFilters=()=>{
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
  };
  providerInput?.addEventListener('input',applyProviderFilters);
  providerButtons.forEach((button)=>button.addEventListener('click',()=>{
    providerGroup=button.dataset.providerGroupFilter;
    providerButtons.forEach((item)=>item.setAttribute('aria-pressed',item===button?'true':'false'));
    applyProviderFilters();
  }));
}

document.querySelectorAll('.doc pre').forEach(pre=>{const btn=document.createElement('button');btn.type='button';btn.className='copy';btn.setAttribute('aria-live','polite');btn.textContent='Copy';btn.addEventListener('click',async()=>{const code=pre.querySelector('code')?.textContent??'';try{await navigator.clipboard.writeText(code);btn.textContent='Copied';btn.classList.add('copied');setTimeout(()=>{btn.textContent='Copy';btn.classList.remove('copied')},1400)}catch{btn.textContent='Failed';setTimeout(()=>{btn.textContent='Copy'},1400)}});pre.appendChild(btn)});

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
