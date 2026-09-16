export function reserveHeadingAnchor(anchors, base) {
  let anchor = base;
  let suffix = 0;
  while (anchors.has(anchor)) {
    suffix += 1;
    anchor = `${base}-${suffix}`;
  }
  anchors.add(anchor);
  return anchor;
}

// This is the site's existing block precedence, not a general Markdown parser.
// In particular, fence markers are recognized even while a comment is open.
export function scanSiteMarkdownLines(markdown) {
  const anchors = new Set();
  let fence = false;
  let comment = false;
  return markdown.replace(/\r\n/g, "\n").split("\n").map((line) => {
    const marker = line.match(/^```(\w+)?\s*$/);
    if (marker) {
      fence = !fence;
      return { line, kind: fence ? "fence-open" : "fence-close", language: marker[1] || "text" };
    }
    if (fence) return { line, kind: "code" };
    if (comment) {
      if (line.includes("-->")) comment = false;
      return { line, kind: "comment" };
    }
    if (line.trimStart().startsWith("<!--")) {
      comment = !line.includes("-->");
      return { line, kind: "comment-start" };
    }
    const heading = line.match(/^(#{1,4})\s+(.+)$/);
    if (heading) {
      const text = heading[2].trim();
      const base = siteHeadingSlug(text);
      return { line, kind: "heading", level: heading[1].length, text, id: base ? reserveHeadingAnchor(anchors, base) : base };
    }
    return { line, kind: "text" };
  });
}

function siteHeadingSlug(text) {
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
  let start = 0;
  let end = out.length;
  while (start < end && out[start] === "-") start += 1;
  while (end > start && out[end - 1] === "-") end -= 1;
  return out.slice(start, end);
}
