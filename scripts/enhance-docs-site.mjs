#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const defaultSiteDir = path.join(process.cwd(), "dist", "docs-site");
const providerGuideRels = new Set([
  "features/providers.md",
  "features/provider-selection.md",
  "features/provider-landscape.md",
  "features/provider-live-smoke.md",
  "features/provider-authoring.md",
  "features/delegated-runner-contract.md",
  "features/capacity-fallback.md",
  "features/aws.md",
  "features/aws-private-workspaces.md",
  "features/azure.md",
  "features/hetzner.md",
  "features/blacksmith-testbox.md",
  "features/namespace-devbox.md",
  "features/namespace-devbox-setup.md",
  "features/semaphore.md",
  "features/sprites.md",
  "features/daytona.md",
  "features/islo.md",
  "features/e2b.md",
  "features/slurm-academic-sandboxes.md",
]);
const providerGuideOutputs = new Set([...providerGuideRels].map((rel) => rel.replace(/\.md$/, ".html")));

export function enhanceDocsPage(source, outRel = "") {
  let enhanced = separateProviderNavigation(source, outRel);
  if (outRel === "features/index.html") enhanced = enhanceFeaturesPage(enhanced);
  return enhanced;
}

export function separateProviderNavigation(source, outRel = "") {
  if (!source.includes('<nav class="sidebar-nav"')) return source;
  const features = readNavSection(source, "Features");
  const providers = readNavSection(source, "Providers");
  if (!features || !providers || providers.body.includes('data-nav-subhead="provider-guides"')) return source;

  const links = features.body.match(/<a class="nav-link[^"]*" href="[^"]+" data-nav-search="[^"]+"(?: aria-current="page")?>[^<]+<\/a>/g) || [];
  const guides = links.filter((link) => [...providerGuideRels].some((rel) => link.includes(rel)));
  if (!guides.length) return source;

  const featureBody = guides.reduce((body, link) => body.replace(link, ""), features.body);
  const providerBody = `<p class="nav-subhead" data-nav-subhead="provider-guides">Provider guides</p>${guides.join("")}<p class="nav-subhead">Adapters</p>${providers.body}`;
  let result = replaceNavSection(source, features, featureBody, links.length - guides.length);
  result = replaceNavSection(result, readNavSection(result, "Providers"), providerBody, providers.count + guides.length);
  result = result.replace(/<details class="nav-section" data-nav-section(?: open)?>[\s\S]*?<\/details>/g, (section) => {
    const active = section.includes('aria-current="page"');
    return section.replace(/<details class="nav-section" data-nav-section(?: open)?>/, `<details class="nav-section" data-nav-section${active ? " open" : ""}>`);
  });
  if (providerGuideOutputs.has(outRel)) {
    result = result.replace('<p class="eyebrow">Features</p>', '<p class="eyebrow">Providers</p>');
  }
  if (!result.includes("/* Provider guide navigation */")) {
    result = result.replace("</style>", `${navigationCss()}\n</style>`);
  }
  return result;
}

export function enhanceFeaturesPage(source) {
  if (source.includes("data-feature-explorer")) return source;
  if (!source.includes("<title>Features - Crabbox Docs</title>")) {
    throw new Error("expected the generated Features page");
  }

  const articleMatch = source.match(/<article class="doc">([\s\S]*?)<\/article>/);
  if (!articleMatch) throw new Error("Features page article was not found");

  const heroMeta = source.match(/<div class="hero-meta">[\s\S]*?<\/div>/)?.[0] || "";
  let article = articleMatch[1]
    .replace(/<h1 id="features">Features<\/h1>\s*/, "")
    .replace(/<nav class="page-nav"[\s\S]*?<\/nav>\s*$/, "");

  const sectionPattern = /<h2 id="([^"]+)">([\s\S]*?)<\/h2>\s*<ul>([\s\S]*?)<\/ul>/g;
  const sections = [];
  article = article.replace(sectionPattern, (_, id, rawHeading, list) => {
    const heading = stripTags(rawHeading.replace(/<a class="anchor"[\s\S]*?<\/a>/, "")).trim();
    const cards = [...list.matchAll(/<li>([\s\S]*?)<\/li>/g)].map((match) => featureCard(match[1], id));
    if (!cards.length) return _;
    sections.push({ id, heading, count: cards.length });
    return `<section class="feature-section" id="${escapeAttr(id)}" data-feature-section data-feature-group="${escapeAttr(id)}">
  <div class="feature-section-head">
    <div><p class="feature-section-kicker">Capability area</p><h2>${escapeHtml(heading)}</h2></div>
    <span class="feature-section-count">${cards.length}</span>
  </div>
  <ul class="feature-card-grid">${cards.join("")}</ul>
</section>`;
  });

  if (!sections.length) throw new Error("Features page capability sections were not found");
  const total = sections.reduce((sum, section) => sum + section.count, 0);
  const firstSection = article.indexOf('<section class="feature-section"');
  const intro = article.slice(0, firstSection).trim();
  const featureSections = article.slice(firstSection).trim();
  article = `${intro}
${featureFilter(sections, total)}
<div class="feature-explorer" data-feature-explorer>${featureSections}</div>`;

  return source
    .replace(/<body>/, '<body class="feature-index">')
    .replace(/<header class="hero">[\s\S]*?<\/header>/, featureHero(heroMeta, total, sections.length))
    .replace('<div class="doc-grid">', '<div class="doc-grid doc-grid-wide doc-grid-features">')
    .replace(articleMatch[0], `<article class="doc doc-wide doc-feature-index">${article}</article>`)
    .replace(/\s*<nav class="toc"[\s\S]*?<\/nav>/, "")
    .replace("</style>", `${featuresCss()}\n</style>`)
    .replace("</script>\n</body>", `${featuresJs()}\n</script>\n</body>`);
}

function readNavSection(source, heading) {
  const pattern = new RegExp(`(<details class="nav-section" data-nav-section(?: open)?>\\s*<summary><h2>${escapeRegExp(heading)}<\\/h2><span class="nav-count">)(\\d+)(<\\/span>[\\s\\S]*?<div class="nav-links">)([\\s\\S]*?)(<\\/div><\\/details>)`);
  const match = source.match(pattern);
  if (!match) return null;
  return { full: match[0], prefix: match[1], count: Number(match[2]), middle: match[3], body: match[4], suffix: match[5] };
}

function replaceNavSection(source, section, body, count) {
  if (!section) return source;
  return source.replace(section.full, `${section.prefix}${count}${section.middle}${body}${section.suffix}`);
}

function featureCard(content, group) {
  const link = content.match(/^\s*<a href="([^"]+)">([\s\S]*?)<\/a>(?:\s*[:—-]\s*)?([\s\S]*)$/);
  const href = link?.[1] || "#";
  const title = link?.[2] || content;
  const description = (link?.[3] || "").trim();
  const search = stripTags(`${title} ${description}`).replace(/\s+/g, " ").trim().toLowerCase();
  return `<li class="feature-card" data-feature-card data-feature-group="${escapeAttr(group)}" data-feature-search="${escapeAttr(search)}">
  <div class="feature-card-top"><a class="feature-card-title" href="${href}">${title}</a><span class="feature-card-arrow" aria-hidden="true">↗</span></div>
  ${description ? `<p>${description}</p>` : ""}
</li>`;
}

function featureFilter(sections, total) {
  const buttons = [["all", "All"], ...sections.map(({ id, heading }) => [id, heading])]
    .map(([id, label], index) => `<button type="button" data-feature-group-filter="${escapeAttr(id)}" aria-pressed="${index === 0 ? "true" : "false"}">${escapeHtml(label)}</button>`)
    .join("");
  return `<div class="feature-filter" data-feature-filter>
  <div class="feature-filter-head"><div><p class="feature-section-kicker">Explore the system</p><label for="feature-filter-input">Find a capability</label></div><output for="feature-filter-input" aria-live="polite" data-feature-count>${total} capabilities</output></div>
  <div class="feature-search"><svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="8.5" cy="8.5" r="5.5"></circle><path d="m13 13 4 4"></path></svg><input id="feature-filter-input" name="feature-filter" type="search" autocomplete="off" spellcheck="false" placeholder="Try cache, desktop, evidence, auth…"></div>
  <div class="feature-filter-groups" role="group" aria-label="Capability area">${buttons}</div>
  <p class="feature-empty" role="status" hidden data-feature-empty>No capabilities match this search.</p>
</div>`;
}

function featureHero(heroMeta, total, groupCount) {
  return `<header class="hero hero-features">
  <div class="feature-hero-top">${heroMeta}</div>
  <div class="feature-hero-copy">
    <p class="eyebrow">Capability reference</p>
    <h1>From local edit to remote proof.</h1>
    <p class="feature-hero-lede">Explore the reusable building blocks behind Crabbox: fleet control, runner access, fast sync, recorded execution, and review-ready evidence.</p>
    <div class="cta"><a class="cta-primary" href="../getting-started.html">Get started</a><a class="cta-secondary" href="../providers/index.html">Choose a provider</a></div>
    <dl class="feature-hero-stats"><div><dt>${total}</dt><dd>capability guides</dd></div><div><dt>${groupCount}</dt><dd>focused areas</dd></div><div><dt>1</dt><dd>remote loop</dd></div></dl>
  </div>
  <div class="feature-flow" aria-label="Crabbox execution flow">
    <div class="feature-flow-line" aria-hidden="true"></div>
    <div class="feature-flow-step"><span>01</span><strong>Lease</strong><small>select or reuse a box</small></div>
    <div class="feature-flow-step"><span>02</span><strong>Sync</strong><small>send only the working diff</small></div>
    <div class="feature-flow-step"><span>03</span><strong>Run</strong><small>stream the command live</small></div>
    <div class="feature-flow-step"><span>04</span><strong>Prove</strong><small>retain results and evidence</small></div>
  </div>
</header>`;
}

function navigationCss() {
  return `
/* Provider guide navigation */
.nav-subhead{margin:10px 10px 4px;color:var(--muted);font-size:.64rem;font-weight:700;text-transform:uppercase;letter-spacing:.08em}
.nav-subhead:first-child{margin-top:2px}
`;
}

function featuresCss() {
  return `
/* Features capability explorer */
body.feature-index main{max-width:1520px}
.hero-features{display:grid;grid-template-columns:minmax(0,1.1fr) minmax(330px,.9fr);align-items:stretch;gap:28px;padding:26px 0 34px;border-bottom:1px solid var(--line)}
.hero-features:after{display:none}
.feature-hero-top{grid-column:1/-1;display:flex;justify-content:flex-end;min-height:34px}
.feature-hero-copy{align-self:center;padding:18px 0 8px}
.hero-features h1{max-width:760px;font-size:clamp(3rem,6vw,5.7rem);line-height:.92;letter-spacing:-.035em}
.feature-hero-lede{max-width:64ch;margin:22px 0 24px;color:var(--body-soft);font-size:1.08rem;line-height:1.65;text-wrap:pretty}
.feature-hero-stats{display:flex;gap:0;margin:30px 0 0;padding:0;max-width:560px;border-top:1px solid var(--line)}
.feature-hero-stats div{flex:1;padding:16px 20px 0 0}
.feature-hero-stats div+div{padding-left:20px;border-left:1px solid var(--line)}
.feature-hero-stats dt{font-family:Fraunces,Georgia,serif;font-size:1.8rem;font-weight:700;line-height:1;color:var(--ink)}
.feature-hero-stats dd{margin:6px 0 0;color:var(--muted);font-size:.75rem;font-weight:700;text-transform:uppercase;letter-spacing:.04em}
.feature-flow{position:relative;display:grid;align-content:center;gap:10px;min-height:420px;padding:34px;background:linear-gradient(145deg,color-mix(in srgb,var(--reef) 13%,var(--paper)),var(--paper) 58%,color-mix(in srgb,var(--coral) 10%,var(--paper)));border:1px solid var(--line);border-radius:20px;overflow:hidden;box-shadow:0 22px 60px rgba(0,0,0,.1)}
.feature-flow:before{content:"";position:absolute;inset:-35% auto auto 48%;width:320px;height:320px;border-radius:50%;background:color-mix(in srgb,var(--coral) 14%,transparent);filter:blur(4px)}
.feature-flow-line{position:absolute;top:72px;bottom:72px;left:59px;width:1px;background:linear-gradient(var(--coral),var(--reef))}
.feature-flow-step{position:relative;display:grid;grid-template-columns:50px minmax(0,1fr);grid-template-rows:auto auto;align-items:center;column-gap:16px;padding:16px 18px;border:1px solid color-mix(in srgb,var(--line) 80%,transparent);border-radius:12px;background:color-mix(in srgb,var(--paper) 88%,transparent);backdrop-filter:blur(10px);box-shadow:0 8px 22px rgba(0,0,0,.05)}
.feature-flow-step span{grid-row:1/3;display:grid;place-items:center;width:34px;height:34px;margin-left:-9px;border-radius:50%;background:var(--ink);color:var(--paper);font:700 .7rem/1 "IBM Plex Mono",monospace;z-index:1}
.feature-flow-step strong{font-family:Fraunces,Georgia,serif;font-size:1.12rem;line-height:1.15}
.feature-flow-step small{color:var(--muted);font-size:.82rem}
.doc-grid-features{margin-top:28px}
.doc-feature-index{max-width:none}
.doc-feature-index>p{max-width:76ch;color:var(--body-soft);font-size:1.04rem}
.doc-feature-index>p a{font-weight:600}
.feature-filter{position:sticky;top:14px;z-index:6;margin:28px 0 34px;padding:18px;border:1px solid var(--line);border-radius:14px;background:color-mix(in srgb,var(--paper) 92%,transparent);box-shadow:0 12px 34px rgba(0,0,0,.08);backdrop-filter:blur(16px)}
.feature-filter-head{display:flex;align-items:flex-end;justify-content:space-between;gap:18px;margin-bottom:10px}
.feature-filter label{display:block;font-family:Fraunces,Georgia,serif;font-size:1.35rem;font-weight:600;line-height:1.1}
.feature-filter output{color:var(--muted);font-size:.86rem;font-variant-numeric:tabular-nums;white-space:nowrap}
.feature-search{position:relative}
.feature-search svg{position:absolute;left:14px;top:50%;width:18px;height:18px;transform:translateY(-50%);fill:none;stroke:var(--muted);stroke-width:1.7;stroke-linecap:round}
.feature-search input{width:100%;border:1px solid var(--line);background:var(--paper);color:var(--ink);border-radius:9px;padding:12px 14px 12px 42px;transition:border-color .15s,box-shadow .15s}
.feature-search input:focus-visible{outline:0;border-color:var(--focus);box-shadow:0 0 0 3px color-mix(in srgb,var(--focus) 18%,transparent)}
.feature-filter-groups{display:flex;gap:7px;margin-top:11px;overflow-x:auto;padding:2px 1px 4px;scrollbar-width:thin}
.feature-filter-groups button{flex:0 0 auto;border:1px solid var(--line);border-radius:999px;background:var(--paper);color:var(--muted);padding:7px 11px;font-size:.78rem;font-weight:600;cursor:pointer;transition:background .15s,border-color .15s,color .15s,transform .15s}
.feature-filter-groups button:hover{border-color:var(--coral);color:var(--ink);transform:translateY(-1px)}
.feature-filter-groups button[aria-pressed="true"]{background:var(--ink);border-color:var(--ink);color:var(--paper)}
.feature-empty{margin:14px 0 0;color:var(--muted)}
.feature-explorer{display:grid;gap:44px}
.feature-section{scroll-margin-top:180px}
.feature-section-head{display:flex;align-items:flex-end;justify-content:space-between;gap:20px;margin-bottom:14px;padding-bottom:12px;border-bottom:1px solid var(--line)}
.feature-section-kicker{margin:0 0 4px!important;color:var(--coral)!important;font-size:.68rem!important;font-weight:700;text-transform:uppercase;letter-spacing:.08em}
.doc-feature-index .feature-section h2{margin:0;font-size:1.85rem}
.feature-section-count{display:grid;place-items:center;min-width:34px;height:28px;padding:0 9px;border-radius:999px;background:var(--inline-bg);color:var(--muted);font-size:.75rem;font-weight:700;font-variant-numeric:tabular-nums}
.feature-card-grid{display:grid!important;grid-template-columns:repeat(3,minmax(0,1fr));gap:12px;padding:0!important;margin:0!important;list-style:none}
.feature-card{position:relative;min-height:142px;margin:0!important;padding:18px 18px 17px;border:1px solid var(--line-soft);border-radius:12px;background:linear-gradient(150deg,var(--panel),color-mix(in srgb,var(--panel) 92%,var(--reef)));overflow:hidden;transition:transform .16s,border-color .16s,box-shadow .16s}
.feature-card:before{content:"";position:absolute;inset:0 auto 0 0;width:3px;background:var(--reef);opacity:.65}
.feature-section:nth-child(even) .feature-card:before{background:var(--coral)}
.feature-section:nth-child(3n) .feature-card:before{background:var(--ochre)}
.feature-card:hover{transform:translateY(-3px);border-color:color-mix(in srgb,var(--reef) 45%,var(--line));box-shadow:0 14px 32px rgba(0,0,0,.09)}
.feature-card-top{display:flex;align-items:flex-start;justify-content:space-between;gap:12px}
.feature-card-title{font-family:Fraunces,Georgia,serif;font-size:1.08rem;font-weight:600;line-height:1.2;color:var(--ink);text-decoration:none}
.feature-card-title:after{content:"";position:absolute;inset:0}
.feature-card-title:hover{color:var(--reef)}
.feature-card-arrow{flex:0 0 auto;color:var(--muted);font-size:1rem;transition:transform .16s,color .16s}
.feature-card:hover .feature-card-arrow{color:var(--coral);transform:translate(2px,-2px)}
.feature-card p{position:relative;margin:11px 0 0;color:var(--body-soft);font-size:.88rem;line-height:1.5;pointer-events:none}
.feature-card p a{position:relative;z-index:2;pointer-events:auto}
@media(max-width:1100px){.hero-features{grid-template-columns:1fr}.feature-flow{min-height:0;grid-template-columns:repeat(4,minmax(0,1fr));padding:22px}.feature-flow-line{left:9%;right:9%;top:49px;bottom:auto;width:auto;height:1px}.feature-flow-step{display:block;padding:48px 14px 14px;text-align:center}.feature-flow-step span{position:absolute;top:12px;left:50%;margin:0;transform:translateX(-50%)}.feature-flow-step strong,.feature-flow-step small{display:block}.feature-flow-step small{margin-top:5px}.feature-card-grid{grid-template-columns:repeat(2,minmax(0,1fr))}}
@media(max-width:700px){.hero-features{padding-top:4px}.feature-hero-top{justify-content:flex-start}.hero-features h1{font-size:clamp(2.8rem,14vw,4.4rem)}.feature-hero-stats{flex-wrap:wrap}.feature-hero-stats div{min-width:33%}.feature-flow{grid-template-columns:1fr;padding:18px}.feature-flow-line{top:45px;bottom:45px;left:42px;right:auto;width:1px;height:auto}.feature-flow-step{display:grid;grid-template-columns:46px minmax(0,1fr);grid-template-rows:auto auto;padding:13px 14px;text-align:left}.feature-flow-step span{position:static;grid-row:1/3;transform:none;margin-left:-5px}.feature-filter{position:static;padding:14px}.feature-filter-head{align-items:flex-start;flex-direction:column;gap:6px}.feature-card-grid{grid-template-columns:1fr}.feature-card{min-height:0}.feature-section{scroll-margin-top:24px}}
`;
}

function featuresJs() {
  return `
const featureFilter=document.querySelector('[data-feature-filter]');
if(featureFilter){
  const featureInput=featureFilter.querySelector('input');
  const featureButtons=[...featureFilter.querySelectorAll('[data-feature-group-filter]')];
  const featureCards=[...document.querySelectorAll('[data-feature-card]')];
  const featureSections=[...document.querySelectorAll('[data-feature-section]')];
  const featureCount=featureFilter.querySelector('[data-feature-count]');
  const featureEmpty=featureFilter.querySelector('[data-feature-empty]');
  let featureGroup='all';
  const applyFeatureFilters=()=>{
    const terms=(featureInput?.value.trim().toLowerCase()||'').split(/\\s+/).filter(Boolean);
    let count=0;
    featureCards.forEach((card)=>{
      const groupMatch=featureGroup==='all'||card.dataset.featureGroup===featureGroup;
      const search=card.dataset.featureSearch||card.textContent.toLowerCase();
      const textMatch=terms.every((term)=>search.includes(term));
      const match=groupMatch&&textMatch;
      card.hidden=!match;
      if(match)count+=1;
    });
    featureSections.forEach((section)=>{section.hidden=![...section.querySelectorAll('[data-feature-card]')].some((card)=>!card.hidden)});
    if(featureCount)featureCount.textContent=count===1?'1 capability':count+' capabilities';
    if(featureEmpty)featureEmpty.hidden=count>0;
  };
  featureInput?.addEventListener('input',applyFeatureFilters);
  featureInput?.addEventListener('keydown',(event)=>{if(event.key==='Escape'&&featureInput.value){featureInput.value='';applyFeatureFilters()}});
  featureButtons.forEach((button)=>button.addEventListener('click',()=>{
    featureGroup=button.dataset.featureGroupFilter;
    featureButtons.forEach((item)=>item.setAttribute('aria-pressed',item===button?'true':'false'));
    applyFeatureFilters();
  }));
}
`;
}

function stripTags(value) {
  return String(value).replace(/<[^>]*>/g, " ").replace(/&(?:amp|lt|gt|quot|#39);/g, " ");
}

function escapeRegExp(value) {
  return String(value).replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function escapeHtml(value) {
  return String(value).replace(/[&<>"']/g, (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[char]);
}

function escapeAttr(value) {
  return escapeHtml(value);
}

function htmlFiles(dir) {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) return htmlFiles(full);
    return entry.name.endsWith(".html") ? [full] : [];
  });
}

function run(siteDir = defaultSiteDir) {
  if (!fs.existsSync(siteDir)) throw new Error(`generated docs site not found: ${siteDir}`);
  let changed = 0;
  for (const file of htmlFiles(siteDir)) {
    const outRel = path.relative(siteDir, file).replaceAll(path.sep, "/");
    const source = fs.readFileSync(file, "utf8");
    const enhanced = enhanceDocsPage(source, outRel);
    if (enhanced === source) continue;
    fs.writeFileSync(file, enhanced, "utf8");
    changed += 1;
  }
  console.log(`enhanced docs site: ${changed} pages`);
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) run(process.argv[2]);
