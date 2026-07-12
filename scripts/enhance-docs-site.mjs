#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

export function enhanceFeaturesPage(source) {
  if (source.includes("data-feature-explorer")) return source;
  if (!source.includes("<title>Features - Crabbox Docs</title>")) return source;

  const articleMatch = source.match(/<article class="doc">([\s\S]*?)<\/article>/);
  if (!articleMatch) throw new Error("Features article not found");
  const meta = source.match(/<div class="hero-meta">[\s\S]*?<\/div>/)?.[0] || "";
  let article = articleMatch[1]
    .replace(/<h1 id="features">Features<\/h1>\s*/, "")
    .replace(/<nav class="page-nav"[\s\S]*?<\/nav>\s*$/, "");

  const groups = [];
  article = article.replace(/<h2 id="([^"]+)">([\s\S]*?)<\/h2>\s*<ul>([\s\S]*?)<\/ul>/g, (full, id, headingHtml, list) => {
    const heading = text(headingHtml.replace(/<a class="anchor"[\s\S]*?<\/a>/, ""));
    const cards = [...list.matchAll(/<li>([\s\S]*?)<\/li>/g)].map((item) => card(item[1], id));
    if (!cards.length) return full;
    groups.push({ id, heading, count: cards.length });
    return `<section class="fx-group" data-fx-group="${attr(id)}"><header><div><small>Capability area</small><h2 id="${attr(id)}">${html(heading)}</h2></div><span>${cards.length}</span></header><ul class="fx-grid">${cards.join("")}</ul></section>`;
  });
  if (!groups.length) throw new Error("Feature groups not found");

  const total = groups.reduce((sum, group) => sum + group.count, 0);
  const first = article.indexOf('<section class="fx-group"');
  article = `${article.slice(0, first).trim()}${filter(groups, total)}<div data-feature-explorer>${article.slice(first).trim()}</div>`;

  const hero = `<header class="hero fx-hero"><div class="fx-meta">${meta}</div><div class="fx-copy"><p class="eyebrow">Capability reference</p><h1>From local edit to remote proof.</h1><p>Explore the building blocks behind Crabbox: shared fleet control, runner access, fast sync, recorded execution, and review-ready evidence.</p><div class="cta"><a class="cta-primary" href="../getting-started.html">Get started</a><a class="cta-secondary" href="../providers/index.html">Choose a provider</a></div><dl><div><dt>${total}</dt><dd>guides</dd></div><div><dt>${groups.length}</dt><dd>areas</dd></div><div><dt>1</dt><dd>remote loop</dd></div></dl></div><div class="fx-flow" aria-label="Crabbox execution flow">${["Lease|select or reuse a box","Sync|send the working diff","Run|stream the command live","Prove|retain results and evidence"].map((step, index) => { const [title, body] = step.split("|"); return `<div><b>0${index + 1}</b><strong>${title}</strong><small>${body}</small></div>`; }).join("")}</div></header>`;

  return source
    .replace("<body>", '<body class="feature-index">')
    .replace(/<header class="hero">[\s\S]*?<\/header>/, hero)
    .replace('<div class="doc-grid">', '<div class="doc-grid doc-grid-wide fx-layout">')
    .replace(articleMatch[0], `<article class="doc doc-wide fx-doc">${article}</article>`)
    .replace(/\s*<nav class="toc"[\s\S]*?<\/nav>/, "")
    .replace("</style>", `${styles()}</style>`)
    .replace("</script>\n</body>", `${script()}</script>\n</body>`);
}

function card(content, group) {
  const match = content.match(/^\s*<a href="([^"]+)">([\s\S]*?)<\/a>(?:\s*[:—-]\s*)?([\s\S]*)$/);
  const href = match?.[1] || "#";
  const title = match?.[2] || content;
  const body = (match?.[3] || "").trim();
  const search = text(`${title} ${body}`).toLowerCase();
  return `<li class="fx-card" data-fx-card data-fx-group="${attr(group)}" data-fx-search="${attr(search)}"><a href="${href}"><span>${title}</span><i aria-hidden="true">↗</i></a>${body ? `<p>${body}</p>` : ""}</li>`;
}

function filter(groups, total) {
  const buttons = [["all", "All"], ...groups.map(({ id, heading }) => [id, heading])]
    .map(([id, label], index) => `<button type="button" data-fx-filter="${attr(id)}" aria-pressed="${index === 0}">${html(label)}</button>`).join("");
  return `<div class="fx-filter"><div class="fx-filter-head"><div><small>Explore the system</small><label for="fx-search">Find a capability</label></div><output aria-live="polite" data-fx-count>${total} capabilities</output></div><div class="fx-search"><span aria-hidden="true">⌕</span><input id="fx-search" type="search" autocomplete="off" spellcheck="false" placeholder="Try cache, desktop, evidence, auth…"></div><div class="fx-pills" role="group" aria-label="Capability area">${buttons}</div><p data-fx-empty hidden>No capabilities match this search.</p></div>`;
}

function styles() { return `
body.feature-index main{max-width:1500px}.fx-hero{display:grid;grid-template-columns:minmax(0,1.08fr) minmax(330px,.92fr);gap:28px;align-items:stretch;padding:24px 0 36px}.fx-hero:after{display:none}.fx-meta{grid-column:1/-1;display:flex;justify-content:flex-end}.fx-copy{align-self:center}.fx-copy h1{font-size:clamp(3rem,6vw,5.6rem);line-height:.92;letter-spacing:-.035em;max-width:780px}.fx-copy>p:not(.eyebrow){max-width:64ch;margin:22px 0 24px;color:var(--body-soft);font-size:1.08rem}.fx-copy dl{display:flex;margin:30px 0 0;border-top:1px solid var(--line);max-width:560px}.fx-copy dl div{flex:1;padding:15px 18px 0 0}.fx-copy dl div+div{padding-left:18px;border-left:1px solid var(--line)}.fx-copy dt{font:700 1.8rem/1 Fraunces,serif}.fx-copy dd{margin:6px 0 0;color:var(--muted);font-size:.72rem;font-weight:700;text-transform:uppercase}.fx-flow{display:grid;align-content:center;gap:10px;min-height:410px;padding:30px;border:1px solid var(--line);border-radius:20px;background:linear-gradient(145deg,color-mix(in srgb,var(--reef) 14%,var(--paper)),var(--paper) 58%,color-mix(in srgb,var(--coral) 10%,var(--paper)));box-shadow:0 22px 60px rgba(0,0,0,.1)}.fx-flow>div{display:grid;grid-template-columns:42px 1fr;grid-template-rows:auto auto;gap:0 14px;padding:15px 17px;border:1px solid color-mix(in srgb,var(--line) 82%,transparent);border-radius:12px;background:color-mix(in srgb,var(--paper) 90%,transparent);backdrop-filter:blur(10px)}.fx-flow b{grid-row:1/3;display:grid;place-items:center;width:34px;height:34px;border-radius:50%;background:var(--ink);color:var(--paper);font:700 .7rem/1 "IBM Plex Mono",monospace}.fx-flow strong{font:600 1.08rem/1.2 Fraunces,serif}.fx-flow small{color:var(--muted)}.fx-layout{margin-top:28px}.fx-doc{max-width:none}.fx-doc>p{max-width:76ch;color:var(--body-soft);font-size:1.04rem}.fx-filter{position:sticky;top:14px;z-index:5;margin:28px 0 36px;padding:17px;border:1px solid var(--line);border-radius:14px;background:color-mix(in srgb,var(--paper) 93%,transparent);box-shadow:0 12px 34px rgba(0,0,0,.08);backdrop-filter:blur(16px)}.fx-filter-head{display:flex;justify-content:space-between;align-items:flex-end;gap:18px;margin-bottom:10px}.fx-filter small,.fx-group header small{display:block;color:var(--coral);font-size:.68rem;font-weight:700;text-transform:uppercase;letter-spacing:.08em}.fx-filter label{font:600 1.35rem/1.1 Fraunces,serif}.fx-filter output{color:var(--muted);font-size:.84rem}.fx-search{position:relative}.fx-search span{position:absolute;left:14px;top:50%;transform:translateY(-50%);color:var(--muted);font-size:1.2rem}.fx-search input{width:100%;padding:12px 14px 12px 42px;border:1px solid var(--line);border-radius:9px;background:var(--paper);color:var(--ink)}.fx-pills{display:flex;gap:7px;margin-top:10px;overflow:auto;padding:2px 1px 4px}.fx-pills button{flex:0 0 auto;padding:7px 11px;border:1px solid var(--line);border-radius:999px;background:var(--paper);color:var(--muted);font-size:.78rem;font-weight:600;cursor:pointer}.fx-pills button:hover{border-color:var(--coral);color:var(--ink)}.fx-pills button[aria-pressed=true]{background:var(--ink);border-color:var(--ink);color:var(--paper)}[data-feature-explorer]{display:grid;gap:44px}.fx-group{scroll-margin-top:180px}.fx-group>header{display:flex;justify-content:space-between;align-items:flex-end;gap:20px;margin-bottom:14px;padding-bottom:12px;border-bottom:1px solid var(--line)}.fx-doc .fx-group h2{margin:0;font-size:1.85rem}.fx-group>header>span{display:grid;place-items:center;min-width:34px;height:28px;padding:0 9px;border-radius:999px;background:var(--inline-bg);color:var(--muted);font-size:.75rem;font-weight:700}.fx-grid{display:grid!important;grid-template-columns:repeat(3,minmax(0,1fr));gap:12px;padding:0!important;margin:0!important;list-style:none}.fx-card{position:relative;min-height:140px;margin:0!important;padding:18px;border:1px solid var(--line-soft);border-radius:12px;background:linear-gradient(150deg,var(--panel),color-mix(in srgb,var(--panel) 92%,var(--reef)));transition:.16s}.fx-card:before{content:"";position:absolute;inset:0 auto 0 0;width:3px;background:var(--reef)}.fx-group:nth-child(even) .fx-card:before{background:var(--coral)}.fx-card:hover{transform:translateY(-3px);border-color:color-mix(in srgb,var(--reef) 45%,var(--line));box-shadow:0 14px 32px rgba(0,0,0,.09)}.fx-card>a{display:flex;justify-content:space-between;gap:12px;color:var(--ink);font:600 1.08rem/1.2 Fraunces,serif;text-decoration:none}.fx-card i{font-style:normal;color:var(--muted)}.fx-card p{position:relative;margin:11px 0 0;color:var(--body-soft);font-size:.88rem;line-height:1.5}@media(max-width:1100px){.fx-hero{grid-template-columns:1fr}.fx-flow{min-height:0;grid-template-columns:repeat(4,1fr);padding:20px}.fx-flow>div{display:block;text-align:center;padding:48px 12px 13px;position:relative}.fx-flow b{position:absolute;top:10px;left:50%;transform:translateX(-50%)}.fx-flow strong,.fx-flow small{display:block}.fx-grid{grid-template-columns:repeat(2,1fr)}}@media(max-width:700px){.fx-meta{justify-content:flex-start}.fx-copy h1{font-size:clamp(2.8rem,14vw,4.3rem)}.fx-flow{grid-template-columns:1fr}.fx-flow>div{display:grid;grid-template-columns:42px 1fr;grid-template-rows:auto auto;text-align:left;padding:13px}.fx-flow b{position:static;grid-row:1/3;transform:none}.fx-filter{position:static;padding:14px}.fx-filter-head{align-items:flex-start;flex-direction:column;gap:6px}.fx-grid{grid-template-columns:1fr}.fx-card{min-height:0}.fx-group{scroll-margin-top:24px}}`;
}

function script() { return `
const fx=document.querySelector('.fx-filter');if(fx){const input=fx.querySelector('input'),buttons=[...fx.querySelectorAll('[data-fx-filter]')],cards=[...document.querySelectorAll('[data-fx-card]')],groups=[...document.querySelectorAll('[data-fx-group]')],count=fx.querySelector('[data-fx-count]'),empty=fx.querySelector('[data-fx-empty]');let group='all';const apply=()=>{const terms=(input.value.trim().toLowerCase()).split(/\\s+/).filter(Boolean);let n=0;cards.forEach(card=>{const show=(group==='all'||card.dataset.fxGroup===group)&&terms.every(term=>(card.dataset.fxSearch||'').includes(term));card.hidden=!show;if(show)n++});groups.forEach(section=>section.hidden=![...section.querySelectorAll('[data-fx-card]')].some(card=>!card.hidden));count.textContent=n===1?'1 capability':n+' capabilities';empty.hidden=n>0};input.addEventListener('input',apply);input.addEventListener('keydown',event=>{if(event.key==='Escape'&&input.value){input.value='';apply()}});buttons.forEach(button=>button.addEventListener('click',()=>{group=button.dataset.fxFilter;buttons.forEach(item=>item.setAttribute('aria-pressed',item===button?'true':'false'));apply()}))}`;
}

function text(value) { return String(value).replace(/<[^>]*>/g, " ").replace(/&(?:amp|lt|gt|quot|#39);/g, " ").replace(/\s+/g, " ").trim(); }
function html(value) { return String(value).replace(/[&<>"']/g, (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[char]); }
function attr(value) { return html(value); }

export function enhanceDocsSite(siteDir = path.join(process.cwd(), "dist", "docs-site")) {
  const file = path.join(siteDir, "features", "index.html");
  if (!fs.existsSync(file)) throw new Error(`generated Features page not found: ${file}`);
  fs.writeFileSync(file, enhanceFeaturesPage(fs.readFileSync(file, "utf8")), "utf8");
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) enhanceDocsSite(process.argv[2]);
