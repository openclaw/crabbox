import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { scanSiteMarkdownLines } from "./markdown-headings.mjs";

export const locales = {
  en: "English", "zh-CN": "简体中文", ja: "日本語", ko: "한국어",
  es: "Español", de: "Deutsch", fr: "Français", "pt-BR": "Português (Brasil)",
};
export const pilotPages = ["README.md", "getting-started.md", "how-it-works.md", "concepts.md", "use-cases.md"];
export const sourceHash = (source) => crypto.createHash("sha256").update(source).digest("hex");

export function readTranslation(file, source) {
  const raw = fs.readFileSync(file, "utf8");
  const metadata = raw.match(/^<!-- source: ([\w/.-]+); sha256: ([a-f0-9]{64}) -->\n/);
  if (!metadata) throw new Error(`Missing translation source/hash: ${file}`);
  return { source: metadata[1], hash: metadata[2], stale: metadata[2] !== sourceHash(source), markdown: raw.slice(metadata[0].length) };
}

export function loadTranslations(root, pages) {
  const translations = new Map();
  for (const locale of Object.keys(locales).filter((lang) => lang !== "en")) {
    for (const page of pages) {
      const file = path.join(root, "docs", ".i18n", locale, page.rel);
      if (!fs.existsSync(file)) continue;
      const translation = readTranslation(file, page.markdown);
      if (translation.source !== page.rel) throw new Error(`Wrong translation source: ${file}`);
      translations.set(`${locale}/${page.rel}`, { ...translation, file });
    }
  }
  return translations;
}

// Translate text and accessible labels only. Executable examples and markup never enter the catalog.
export function localizeHTML(html, messages) {
  const message = (key) => Object.hasOwn(messages, key) ? messages[key] : undefined;
  const translate = (text) => {
    const trimmed = text.trim();
    const normalized = trimmed.replace(/\b\d+\b/g, "{count}");
    const template = message(trimmed) ?? message(normalized);
    const numbers = trimmed.match(/\b\d+\b/g) || [];
    let index = 0;
    const translated = template?.replaceAll("{count}", () => numbers[index++] ?? "{count}");
    if (translated === undefined) return text;
    return text.replace(trimmed, () => escapeText(translated));
  };
  return html.split(/(<pre\b[\s\S]*?<\/pre>|<code\b[\s\S]*?<\/code>|<script\b[\s\S]*?<\/script>|<style\b[\s\S]*?<\/style>|<[^>]+>)/g)
    .map((part) => part.startsWith("<")
      ? (/^<(?:pre|code|script|style)\b/.test(part) ? part : part.replace(/\b(aria-label|placeholder|title|data-copy-label|content)="([^"]*)"/g,
        (match, key, value) => message(value) === undefined ? match : `${key}="${escapeText(message(value))}"`))
      : translate(part)).join("");
}

function escapeText(value) {
  return value.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;").replaceAll('"', "&quot;");
}

export function protectedMarkdown(markdown) {
  const blocks = [];
  let fence = [];
  const prose = [];
  for (const entry of scanSiteMarkdownLines(markdown)) {
    if (entry.kind === "fence-open") fence = [entry.line];
    else if (entry.kind === "fence-close") { fence.push(entry.line); blocks.push(fence.join("\n")); }
    else if (entry.kind === "code") fence.push(entry.line);
    else if (entry.kind === "text" || entry.kind === "heading") prose.push(entry.line);
  }
  const text = prose.join("\n");
  return {
    blocks,
    inline: [...text.matchAll(/`([^`]+)`/g)].map((m) => m[1]).sort(),
    links: [...text.matchAll(/\]\(([^)]+)\)|<(https?:\/\/[^>]+)>/g)].map((m) => m[1] || m[2]).sort(),
    headings: scanSiteMarkdownLines(markdown).filter((entry) => entry.kind === "heading").map(({ level, id }) => ({ level, id })),
  };
}
