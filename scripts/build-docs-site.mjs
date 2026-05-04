#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";

const root = process.cwd();
const docsDir = path.join(root, "docs");
const outDir = path.join(root, "dist", "docs-site");
const repoEditBase = "https://github.com/openclaw/crabbox/edit/main/docs";

const sections = [
  ["Start", ["README.md", "how-it-works.md", "architecture.md", "orchestrator.md", "cli.md"]],
  ["Features", rels("features")],
  ["Commands", rels("commands")],
  [
    "Operate",
    ["operations.md", "observability.md", "troubleshooting.md", "performance.md", "infrastructure.md", "security.md", "mvp-plan.md"],
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
console.log(`built docs site: ${path.relative(root, outDir)}`);

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

function markdownToHtml(markdown, currentRel) {
  const lines = markdown.replace(/\r\n/g, "\n").split("\n");
  const html = [];
  let paragraph = [];
  let list = null;
  let fence = null;

  const flushParagraph = () => {
    if (!paragraph.length) return;
    html.push(`<p>${inline(paragraph.join(" "), currentRel)}</p>`);
    paragraph = [];
  };
  const closeList = () => {
    if (!list) return;
    html.push(`</${list}>`);
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
      const th = header.map((c, idx) => `<th${aligns[idx] ? ` style="text-align:${aligns[idx]}"` : ""}>${inline(c, currentRel)}</th>`).join("");
      const tb = rows.map((r) => `<tr>${r.map((c, idx) => `<td${aligns[idx] ? ` style="text-align:${aligns[idx]}"` : ""}>${inline(c, currentRel)}</td>`).join("")}</tr>`).join("");
      html.push(`<table><thead><tr>${th}</tr></thead><tbody>${tb}</tbody></table>`);
      continue;
    }
    const bullet = line.match(/^\s*-\s+(.+)$/);
    const numbered = line.match(/^\s*\d+\.\s+(.+)$/);
    if (bullet || numbered) {
      flushParagraph();
      const tag = bullet ? "ul" : "ol";
      if (list && list !== tag) closeList();
      if (!list) {
        list = tag;
        html.push(`<${tag}>`);
      }
      html.push(`<li>${inline((bullet || numbered)[1], currentRel)}</li>`);
      continue;
    }
    paragraph.push(line.trim());
  }
  flushParagraph();
  closeList();
  return html.join("\n");
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
  const re = /<h([23]) id="([^"]+)">([\s\S]*?)<\/h[23]>/g;
  let m;
  while ((m = re.exec(html))) {
    const text = m[3]
      .replace(/<a class="anchor"[^>]*>.*?<\/a>/, "")
      .replace(/<[^>]+>/g, "")
      .trim();
    items.push({ level: Number(m[1]), id: m[2], text });
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
  const prevNext = !isHome && (prev || next) ? pageNavHtml(prev, next, rootPrefix) : "";
  const heroBlock = isHome ? landingHero(rootPrefix) : standardHero(page, sectionName, editUrl);
  const articleClass = isHome ? "doc doc-home" : "doc";
  const tocBlock = isHome ? "" : toc;
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>${escapeHtml(page.title)} - Crabbox Docs</title>
  <link rel="icon" href="${rootPrefix}crabbox.svg">
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=Fraunces:wght@600;700&family=IBM+Plex+Sans:wght@400;500;600;700&family=IBM+Plex+Mono:wght@500;600&display=swap" rel="stylesheet">
  <style>${css()}</style>
</head>
<body${isHome ? ' class="home"' : ""}>
  <button class="nav-toggle" type="button" aria-label="Toggle navigation" aria-expanded="false">
    <span aria-hidden="true"></span><span aria-hidden="true"></span><span aria-hidden="true"></span>
  </button>
  <div class="shell">
    <aside class="sidebar">
      <a class="brand" href="${rootPrefix}index.html" aria-label="Crabbox docs home">
        <img src="${rootPrefix}crabbox.svg" alt="">
        <span><strong>Crabbox</strong><small>Remote testbox docs</small></span>
      </a>
      <label class="search"><span>Search docs</span><input id="doc-search" type="search" placeholder="leases, cost, ssh"></label>
      <nav>${navHtml(page.rel, rootPrefix)}</nav>
    </aside>
    <main>
      ${heroBlock}
      <div class="doc-grid${isHome ? " doc-grid-home" : ""}">
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
    ["Local loop, remote box", "Keep your editor and git workflow. Crabbox rsyncs your dirty checkout to a leased Linux machine and streams the run back."],
    ["Brokered, not BYO creds", "A Cloudflare Worker holds provider credentials and serializes lease state. Your CLI only carries a bearer token."],
    ["Cost-aware leases", "TTL-bounded machines, monthly spend caps, and per-user / per-org / per-provider usage from the broker."],
    ["Reuse what's warm", "<code>crabbox warmup</code> keeps a box hot. Reuse it with <code>--id</code> across runs, SSH, and CI hydration."],
    ["Hetzner or AWS Spot", "Falls back across server types and instance families when capacity is tight. Direct provider mode stays as a debug fallback."],
    ["Plays with Actions", "<code>actions hydrate</code> reuses your repository's GitHub Actions setup steps so local runs land in the same hydrated workspace."],
  ];
  const cards = features
    .map(([title, body]) => `<article class="feature"><h3>${escapeHtml(title)}</h3><p>${body}</p></article>`)
    .join("");
  return `<header class="hero hero-home">
        <div class="hero-text">
          <p class="eyebrow">OpenClaw - remote testbox</p>
          <h1>A short-lived Linux box for every <em>run</em>.</h1>
          <p class="lede">Crabbox gives maintainers and agents a fast local loop on shared cloud capacity: lease, sync, run, release. The CLI keeps the developer story simple; a Cloudflare-hosted broker keeps the fleet safe.</p>
          <div class="cta">
            <a class="cta-primary" href="${rootPrefix}how-it-works.html">Read the overview</a>
            <a class="cta-secondary" href="https://github.com/openclaw/crabbox" rel="noopener">View on GitHub</a>
          </div>
        </div>
        <pre class="hero-snippet" aria-hidden="true"><code><span class="prompt">$</span> crabbox run -- pnpm test
<span class="comment"># lease cbx_8f2 - hetzner cax21 - ready 11s</span>
<span class="comment"># sync 184 files (1.2 MB)</span>
<span class="comment"># tests passed in 47s - released</span></code></pre>
      </header>
      <section class="features" aria-label="Highlights">${cards}</section>`;
}

function pageNavHtml(prev, next, rootPrefix) {
  const cell = (page, dir) => {
    if (!page) return "";
    return `<a class="page-nav-${dir}" href="${rootPrefix}${page.outRel}"><small>${dir === "prev" ? "Previous" : "Next"}</small><span>${escapeHtml(page.title)}</span></a>`;
  };
  return `<nav class="page-nav" aria-label="Pager">${cell(prev, "prev")}${cell(next, "next")}</nav>`;
}

function navHtml(currentRel, rootPrefix) {
  return nav
    .map((section) => `<section><h2>${section.name}</h2>${section.pages.map((page) => {
      const href = rootPrefix + page.outRel;
      const active = page.rel === currentRel ? " active" : "";
      return `<a class="nav-link${active}" href="${href}">${escapeHtml(page.title)}</a>`;
    }).join("")}</section>`)
    .join("");
}

function css() {
  return `
:root{--ink:#12211f;--muted:#5d6e69;--shell:#f7efe3;--paper:#fffbf4;--reef:#145f58;--tide:#1f7b93;--coral:#e35e46;--ochre:#c89231;--line:#dfd2c0;--line-soft:#ebdfca;--shadow:0 18px 48px rgba(18,33,31,.10)}
*{box-sizing:border-box}
html{scroll-behavior:smooth;scroll-padding-top:24px}
body{margin:0;background:var(--shell);color:var(--ink);font-family:"IBM Plex Sans",Avenir Next,sans-serif;line-height:1.65;overflow-x:hidden;-webkit-font-smoothing:antialiased}
body:before{content:"";position:fixed;inset:0;pointer-events:none;background:linear-gradient(90deg,rgba(18,33,31,.045) 1px,transparent 1px),linear-gradient(rgba(18,33,31,.035) 1px,transparent 1px);background-size:44px 44px;mask-image:linear-gradient(90deg,#000,transparent 72%)}
::selection{background:var(--coral);color:var(--paper)}
a{color:var(--reef);text-decoration-thickness:.07em;text-underline-offset:.18em;transition:color .15s}
a:hover{color:var(--coral)}
.shell{display:grid;grid-template-columns:280px minmax(0,1fr);min-height:100vh}

/* sidebar */
.sidebar{position:sticky;top:0;height:100vh;overflow:auto;padding:24px 20px;background:rgba(255,251,244,.86);border-right:1px solid var(--line);backdrop-filter:blur(20px);scrollbar-width:thin;scrollbar-color:var(--line) transparent}
.sidebar::-webkit-scrollbar{width:6px}
.sidebar::-webkit-scrollbar-thumb{background:var(--line);border-radius:6px}
.brand{display:flex;align-items:center;gap:11px;color:var(--ink);text-decoration:none;margin-bottom:22px}
.brand img{width:42px;height:42px}
.brand strong{display:block;font-family:Fraunces,serif;font-size:1.32rem;line-height:1;letter-spacing:.005em}
.brand small{display:block;color:var(--muted);font-size:.74rem;margin-top:4px}
.search{display:block;margin:0 0 22px}
.search span{display:block;color:var(--muted);font-size:.7rem;font-weight:700;text-transform:uppercase;letter-spacing:.1em;margin-bottom:7px}
.search input{width:100%;border:1px solid var(--line);background:var(--paper);border-radius:8px;padding:10px 12px;font:inherit;font-size:.92rem;color:var(--ink);outline:none;transition:border-color .15s,box-shadow .15s}
.search input:focus{border-color:var(--coral);box-shadow:0 0 0 3px rgba(227,94,70,.18)}
nav section{margin:0 0 20px}
nav h2{font-size:.68rem;color:var(--muted);text-transform:uppercase;letter-spacing:.12em;margin:0 0 6px;font-weight:700}
.nav-link{display:block;color:var(--ink);text-decoration:none;border-radius:7px;padding:6px 10px;margin:1px 0;font-size:.91rem;line-height:1.4;border-left:2px solid transparent;transition:background .12s,color .12s}
.nav-link:hover{background:rgba(227,94,70,.08);color:var(--reef)}
.nav-link.active{background:#efe2d0;color:#0e423c;border-left-color:var(--coral);font-weight:600}

/* main */
main{min-width:0;padding:28px clamp(20px,4.5vw,60px) 72px;max-width:1180px;margin:0 auto;width:100%}
.hero{display:flex;align-items:flex-end;justify-content:space-between;gap:22px;border-bottom:1px solid var(--line);padding:18px 0 22px;position:relative;flex-wrap:wrap}
.hero:after{content:"";position:absolute;left:0;bottom:-1px;width:88px;height:3px;background:linear-gradient(90deg,var(--coral),var(--ochre),var(--tide));border-radius:3px}
.hero-text{min-width:0;flex:1 1 320px}
.eyebrow{margin:0 0 8px;color:var(--coral);font-weight:700;text-transform:uppercase;letter-spacing:.14em;font-size:.72rem}
.hero h1{font-family:Fraunces,Georgia,serif;font-size:clamp(1.9rem,3.4vw,2.85rem);line-height:1.05;letter-spacing:-.005em;margin:0;font-weight:700;color:var(--ink)}
.hero-meta{display:flex;gap:8px;flex:0 0 auto}
.repo,.edit{border:1px solid var(--line);color:var(--ink);text-decoration:none;border-radius:8px;padding:7px 12px;font-weight:600;font-size:.84rem;background:var(--paper);transition:border-color .15s,color .15s}
.repo:hover,.edit:hover{border-color:var(--coral);color:var(--coral)}
.edit{color:var(--muted)}

/* landing hero */
.hero-home{display:grid;grid-template-columns:minmax(0,1.15fr) minmax(0,1fr);gap:36px;align-items:center;border-bottom:0;padding:24px 0 12px}
.hero-home:after{display:none}
.hero-home .eyebrow{margin-bottom:14px}
.hero-home h1{font-size:clamp(2.1rem,4.6vw,3.8rem);line-height:1.02;letter-spacing:-.012em;font-weight:700;margin:0 0 16px;max-width:18ch}
.hero-home h1 em{font-style:italic;color:var(--coral);font-weight:600}
.lede{margin:0 0 22px;color:#384744;font-size:clamp(1rem,1.25vw,1.1rem);line-height:1.55;max-width:46ch}
.cta{display:flex;gap:10px;flex-wrap:wrap}
.cta-primary,.cta-secondary{display:inline-flex;align-items:center;border-radius:9px;padding:10px 16px;font-weight:600;font-size:.93rem;text-decoration:none;transition:transform .15s,box-shadow .15s,background .15s,border-color .15s,color .15s}
.cta-primary{background:var(--ink);color:var(--paper);border:1px solid var(--ink)}
.cta-primary:hover{background:var(--reef);border-color:var(--reef);color:var(--paper);transform:translateY(-1px);box-shadow:0 8px 20px rgba(20,95,88,.25)}
.cta-secondary{border:1px solid var(--ink);color:var(--ink);background:transparent}
.cta-secondary:hover{border-color:var(--coral);color:var(--coral);transform:translateY(-1px)}
.hero-snippet{margin:0;background:#0f1c1a;color:#f8efe4;border-radius:12px;padding:22px 22px;font:500 .88rem/1.65 "IBM Plex Mono",ui-monospace,monospace;border:1px solid #0a1513;box-shadow:0 18px 40px rgba(18,33,31,.18);overflow:hidden}
.hero-snippet code{background:transparent;border:0;padding:0;color:inherit;font:inherit;display:block;white-space:pre}
.hero-snippet .prompt{color:var(--ochre)}
.hero-snippet .comment{color:#7e948f}

/* feature grid */
.features{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:14px;margin:30px 0 8px}
.feature{background:rgba(255,251,244,.78);border:1px solid var(--line-soft);border-radius:10px;padding:18px 18px 16px;transition:border-color .15s,transform .15s,box-shadow .15s}
.feature:hover{border-color:var(--coral);transform:translateY(-2px);box-shadow:0 10px 24px rgba(18,33,31,.08)}
.feature h3{font-family:Fraunces,Georgia,serif;font-size:1.05rem;margin:0 0 6px;font-weight:600;letter-spacing:-.005em;line-height:1.2}
.feature p{margin:0;color:#384744;font-size:.92rem;line-height:1.5}
.feature code{font-size:.86em;background:#efe2d0;border:1px solid #e2d2bd;border-radius:5px;padding:.04em .3em}

/* layout: doc + toc */
.doc-grid{display:grid;grid-template-columns:minmax(0,1fr);gap:36px;margin-top:30px}
.doc-grid-home{margin-top:14px}
.doc-home{background:transparent;box-shadow:none;border:0;padding:8px clamp(18px,3vw,30px) 0;max-width:74ch;margin-inline:auto;width:100%}
.doc-home>:first-child{margin-top:0}
@media(min-width:1180px){.doc-grid{grid-template-columns:minmax(0,74ch) 200px;justify-content:start}.doc-grid-home{grid-template-columns:minmax(0,1fr)}}
.doc{min-width:0;max-width:74ch;background:rgba(255,251,244,.78);box-shadow:var(--shadow);border:1px solid var(--line-soft);border-radius:10px;padding:clamp(22px,3.6vw,44px);overflow-wrap:break-word}
.doc-home{max-width:none}
.doc h1{display:none}
.doc h2{font-family:Fraunces,Georgia,serif;font-size:1.65rem;line-height:1.15;margin:1.9em 0 .5em;font-weight:600;letter-spacing:-.005em;position:relative}
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
.doc code{font-family:"IBM Plex Mono",ui-monospace,monospace;font-size:.86em;background:#efe2d0;border:1px solid #e2d2bd;border-radius:5px;padding:.08em .34em}
.doc pre{position:relative;overflow:auto;background:#0f1c1a;color:#f8efe4;border-radius:9px;padding:16px 20px;border:1px solid #0a1513;box-shadow:inset 0 0 0 1px rgba(255,255,255,.03);margin:1.35em 0;font-size:.88em;scrollbar-width:thin;scrollbar-color:#3a4a47 transparent}
.doc pre::-webkit-scrollbar{height:8px}
.doc pre::-webkit-scrollbar-thumb{background:#3a4a47;border-radius:8px}
.doc pre code{display:block;background:transparent;border:0;color:inherit;padding:0;font-size:1em;white-space:pre-wrap;overflow-wrap:anywhere}
.doc pre .copy{position:absolute;top:8px;right:8px;background:rgba(255,251,244,.06);color:#f8efe4;border:1px solid rgba(255,251,244,.18);border-radius:6px;padding:3px 9px;font:600 .7rem/1 "IBM Plex Sans",sans-serif;cursor:pointer;opacity:0;transition:opacity .15s,background .15s,border-color .15s}
.doc pre:hover .copy,.doc pre .copy:focus{opacity:1}
.doc pre .copy:hover{background:rgba(255,251,244,.14)}
.doc pre .copy.copied{background:var(--coral);border-color:var(--coral);opacity:1}
.doc blockquote{margin:1.4em 0;padding:12px 16px;border-left:3px solid var(--coral);background:#f0e3d3;border-radius:0 8px 8px 0;color:var(--ink)}
.doc blockquote p:last-child{margin-bottom:0}
.doc table{width:100%;border-collapse:collapse;margin:1.2em 0;font-size:.94em}
.doc th,.doc td{border-bottom:1px solid var(--line);padding:9px 10px;text-align:left}
.doc th{font-weight:600;color:var(--reef)}
.doc hr{border:0;border-top:1px solid var(--line);margin:2em 0}

/* toc */
.toc{position:sticky;top:24px;align-self:start;font-size:.85rem;padding-left:14px;border-left:1px solid var(--line);max-height:calc(100vh - 48px);overflow:auto;scrollbar-width:thin;scrollbar-color:var(--line) transparent}
.toc::-webkit-scrollbar{width:5px}
.toc::-webkit-scrollbar-thumb{background:var(--line);border-radius:5px}
.toc h2{font-size:.66rem;color:var(--muted);text-transform:uppercase;letter-spacing:.12em;margin:0 0 10px;font-weight:700}
.toc a{display:block;color:var(--muted);text-decoration:none;padding:4px 0 4px 10px;line-height:1.35;border-left:2px solid transparent;margin-left:-12px;transition:color .12s,border-color .12s}
.toc a:hover{color:var(--ink)}
.toc a.active{color:var(--reef);border-left-color:var(--coral);font-weight:600}
.toc-l3{padding-left:22px!important;font-size:.94em}
@media(max-width:1179px){.toc{display:none}}

/* prev/next pager */
.page-nav{display:grid;grid-template-columns:1fr 1fr;gap:14px;margin-top:48px}
.page-nav>a{display:block;border:1px solid var(--line);background:var(--paper);border-radius:10px;padding:14px 18px;text-decoration:none;color:var(--ink);transition:border-color .15s,transform .15s,box-shadow .15s}
.page-nav>a:hover{border-color:var(--coral);transform:translateY(-1px);box-shadow:0 6px 18px rgba(18,33,31,.08)}
.page-nav small{display:block;color:var(--muted);font-size:.7rem;text-transform:uppercase;letter-spacing:.12em;margin-bottom:5px;font-weight:700}
.page-nav span{display:block;font-weight:600;line-height:1.3}
.page-nav-prev{text-align:left}
.page-nav-next{text-align:right;grid-column:2}
.page-nav-prev:only-child{grid-column:1}

/* mobile nav toggle */
.nav-toggle{display:none;position:fixed;top:14px;right:14px;top:calc(14px + env(safe-area-inset-top, 0px));right:calc(14px + env(safe-area-inset-right, 0px));z-index:20;width:40px;height:40px;border-radius:9px;background:var(--paper);border:1px solid var(--line);color:var(--ink);cursor:pointer;padding:10px 9px;flex-direction:column;align-items:stretch;justify-content:space-between;box-shadow:0 6px 18px rgba(18,33,31,.12)}
.nav-toggle span{display:block;width:100%;height:2px;flex:0 0 2px;background:currentColor;border-radius:2px;transition:transform .2s,opacity .2s}
.nav-toggle[aria-expanded="true"] span:nth-child(1){transform:translateY(8px) rotate(45deg)}
.nav-toggle[aria-expanded="true"] span:nth-child(2){opacity:0}
.nav-toggle[aria-expanded="true"] span:nth-child(3){transform:translateY(-8px) rotate(-45deg)}

/* mobile */
@media(max-width:900px){
  .shell{display:block}
  .sidebar{position:fixed;inset:0 30% 0 0;max-width:320px;height:100vh;z-index:15;transform:translateX(-100%);transition:transform .25s ease;box-shadow:0 18px 40px rgba(18,33,31,.18);background:var(--paper);pointer-events:none}
  .sidebar.open{transform:translateX(0);pointer-events:auto}
  .nav-toggle{display:flex}
  main{padding:64px 18px 56px}
  .hero{padding-top:8px}
  .hero h1{font-size:clamp(1.7rem,7vw,2.2rem)}
  .hero-meta{width:100%;justify-content:flex-start}
  .hero-home{grid-template-columns:1fr;gap:22px}
  .hero-home h1{font-size:clamp(2rem,8vw,2.6rem);max-width:none}
  .hero-snippet{font-size:.78rem;padding:16px 16px}
  .features{grid-template-columns:1fr;margin-top:22px}
  .doc{padding:20px;border-radius:8px}
  .doc-home{padding:0 18px}
  .doc-grid{margin-top:22px;gap:24px}
  .doc :is(h2,h3,h4) .anchor{display:none}
}
@media(max-width:520px){
  main{padding:60px 14px 48px}
  .doc{padding:18px 16px}
  .doc-home{padding-inline:16px}
  .doc pre{margin-left:-16px;margin-right:-16px;border-radius:0;border-left:0;border-right:0}
}
`;
}

function js() {
  return `
const sidebar=document.querySelector('.sidebar');
const toggle=document.querySelector('.nav-toggle');
const mobileNav=window.matchMedia('(max-width: 900px)');
const sidebarFocusable='a[href],button,input,select,textarea,[tabindex]';
function setSidebarFocusable(enabled){
  sidebar?.querySelectorAll(sidebarFocusable).forEach((el)=>{
    if(enabled){
      if(el.dataset.sidebarTabindex!==undefined){
        if(el.dataset.sidebarTabindex)el.setAttribute('tabindex',el.dataset.sidebarTabindex);
        else el.removeAttribute('tabindex');
        delete el.dataset.sidebarTabindex;
      }
    }else if(el.dataset.sidebarTabindex===undefined){
      el.dataset.sidebarTabindex=el.getAttribute('tabindex')??'';
      el.setAttribute('tabindex','-1');
    }
  });
}
function setSidebarOpen(open){
  if(!sidebar||!toggle)return;
  sidebar.classList.toggle('open',open);
  toggle.setAttribute('aria-expanded',open?'true':'false');
  if(mobileNav.matches){
    sidebar.inert=!open;
    if(open)sidebar.removeAttribute('aria-hidden');
    else sidebar.setAttribute('aria-hidden','true');
    setSidebarFocusable(open);
  }else{
    sidebar.inert=false;
    sidebar.removeAttribute('aria-hidden');
    setSidebarFocusable(true);
  }
}
setSidebarOpen(false);
toggle?.addEventListener('click',()=>setSidebarOpen(!sidebar?.classList.contains('open')));
document.addEventListener('click',(e)=>{if(!sidebar?.classList.contains('open'))return;if(sidebar.contains(e.target)||toggle?.contains(e.target))return;setSidebarOpen(false)});
document.addEventListener('keydown',(e)=>{if(e.key==='Escape')setSidebarOpen(false)});
const syncSidebarForViewport=()=>setSidebarOpen(sidebar?.classList.contains('open')??false);
if(mobileNav.addEventListener)mobileNav.addEventListener('change',syncSidebarForViewport);
else mobileNav.addListener?.(syncSidebarForViewport);

const input=document.getElementById('doc-search');
input?.addEventListener('input',()=>{const q=input.value.trim().toLowerCase();document.querySelectorAll('nav section').forEach(sec=>{let any=false;sec.querySelectorAll('.nav-link').forEach(a=>{const m=!q||a.textContent.toLowerCase().includes(q);a.style.display=m?'block':'none';if(m)any=true});sec.style.display=any?'block':'none'})});

document.querySelectorAll('.doc pre').forEach(pre=>{const btn=document.createElement('button');btn.type='button';btn.className='copy';btn.textContent='Copy';btn.addEventListener('click',async()=>{const code=pre.querySelector('code')?.textContent??'';try{await navigator.clipboard.writeText(code);btn.textContent='Copied';btn.classList.add('copied');setTimeout(()=>{btn.textContent='Copy';btn.classList.remove('copied')},1400)}catch{btn.textContent='Failed';setTimeout(()=>{btn.textContent='Copy'},1400)}});pre.appendChild(btn)});

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
  return text.toLowerCase().replace(/`/g, "").replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
}

function escapeHtml(value) {
  return String(value).replace(/[&<>"']/g, (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[char]);
}

function escapeAttr(value) {
  return escapeHtml(value);
}
