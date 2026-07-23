#!/usr/bin/env node
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { chromium } from "playwright";

const siteDir = path.resolve(process.argv[2] || "dist/docs-site");
const outDir = path.resolve(process.argv[3] || "dist/ui-proof");
const explicitPort = process.env.CRABBOX_DOCS_PROOF_PORT !== undefined;
const preferredPort = explicitPort ? Number(process.env.CRABBOX_DOCS_PROOF_PORT) : 4173;

if (!Number.isInteger(preferredPort) || preferredPort < 0 || preferredPort > 65535) {
  throw new Error(`invalid CRABBOX_DOCS_PROOF_PORT: ${process.env.CRABBOX_DOCS_PROOF_PORT}`);
}

const featuresPage = path.join(siteDir, "features", "index.html");
if (!fs.existsSync(featuresPage)) {
  throw new Error(`generated Features page not found: ${featuresPage}`);
}
const homePage = path.join(siteDir, "index.html");
if (!fs.existsSync(homePage)) {
  throw new Error(`generated homepage not found: ${homePage}`);
}

fs.mkdirSync(outDir, { recursive: true });
for (const artifact of [
  "features-desktop-light.png",
  "features-search-desktop.png",
  "features-filter-desktop.png",
  "features-empty-desktop.png",
  "features-desktop-dark.png",
  "features-mobile.png",
  "home-desktop-light.png",
  "home-desktop-dark.png",
  "home-mobile.png",
  "interaction-proof.json",
  "SHA256SUMS",
]) {
  fs.rmSync(path.join(outDir, artifact), { force: true });
}

const server = http.createServer((request, response) => {
  let pathname;
  try {
    pathname = decodeURIComponent(new URL(request.url || "/", "http://127.0.0.1").pathname);
  } catch {
    response.writeHead(400).end("bad request");
    return;
  }

  if (pathname.endsWith("/")) pathname += "index.html";
  const requestedPath = pathname.startsWith("/") ? pathname.slice(1) : pathname;
  const file = path.resolve(siteDir, requestedPath);
  const relative = path.relative(siteDir, file);
  if (relative.startsWith("..") || path.isAbsolute(relative)) {
    response.writeHead(403).end("forbidden");
    return;
  }

  fs.readFile(file, (error, data) => {
    if (error) {
      response.writeHead(404).end("not found");
      return;
    }
    response.writeHead(200, { "content-type": contentType(file), "cache-control": "no-store" });
    response.end(data);
  });
});

const baseURL = await listen(server, preferredPort, explicitPort);
const proof = {
  subject: "Docs browser proof",
  source: {
    commit: process.env.CRABBOX_DOCS_PROOF_COMMIT || process.env.GITHUB_SHA || null,
    workflowCommit: process.env.GITHUB_SHA || null,
    ref: process.env.GITHUB_REF || null,
    event: process.env.GITHUB_EVENT_NAME || null,
    runId: process.env.GITHUB_RUN_ID || null,
  },
  url: `${baseURL}/features/`,
  homeUrl: `${baseURL}/`,
  siteDir: path.relative(process.cwd(), siteDir) || ".",
  outDir: path.relative(process.cwd(), outDir) || ".",
  screenshots: [],
  assertions: [],
  interactions: {},
};

const browser = await chromium.launch({ headless: true });
let activeProofPage;
let failure;
try {
  const desktopLight = await openProofPage(browser, {
    name: "desktop light",
    colorScheme: "light",
    viewport: { width: 1440, height: 1100 },
  });
  activeProofPage = desktopLight;
  await assertFeatureShell(desktopLight.page);
  await assertHandoff(desktopLight.page);
  await screenshot(desktopLight.page, "features-desktop-light.png");
  await proveSlashSearch(desktopLight.page);
  await screenshot(desktopLight.page, "features-search-desktop.png");
  await proveFilter(desktopLight.page);
  await screenshot(desktopLight.page, "features-filter-desktop.png");
  await proveDeepLinkedState(desktopLight.page);
  await proveEscapeClears(desktopLight.page);
  await proveEmptyResult(desktopLight.page);
  await screenshot(desktopLight.page, "features-empty-desktop.png");
  await openHome(desktopLight.page);
  await assertHomeShell(desktopLight.page, "desktop light");
  await proveHomeJobFinder(desktopLight.page);
  await openHome(desktopLight.page);
  await assertHomeShell(desktopLight.page, "desktop light restored");
  await screenshot(desktopLight.page, "home-desktop-light.png");
  await assertNoPageErrors(desktopLight);
  await desktopLight.context.close();
  activeProofPage = undefined;

  const desktopDark = await openProofPage(browser, {
    name: "desktop dark",
    colorScheme: "dark",
    viewport: { width: 1440, height: 1100 },
  });
  activeProofPage = desktopDark;
  await assertTheme(desktopDark.page, "dark");
  await assertFeatureShell(desktopDark.page);
  await screenshot(desktopDark.page, "features-desktop-dark.png");
  await openHome(desktopDark.page);
  await assertTheme(desktopDark.page, "dark");
  await assertHomeShell(desktopDark.page, "desktop dark");
  await screenshot(desktopDark.page, "home-desktop-dark.png");
  await assertNoPageErrors(desktopDark);
  await desktopDark.context.close();
  activeProofPage = undefined;

  const mobile = await openProofPage(browser, {
    name: "mobile light",
    colorScheme: "light",
    isMobile: true,
    viewport: { width: 390, height: 844 },
  });
  activeProofPage = mobile;
  await assertTheme(mobile.page, "light");
  await assertMobileLayout(mobile.page);
  await screenshot(mobile.page, "features-mobile.png");
  await openHome(mobile.page);
  await assertTheme(mobile.page, "light");
  await assertHomeShell(mobile.page, "mobile light");
  await screenshot(mobile.page, "home-mobile.png");
  await assertNoPageErrors(mobile);
  await mobile.context.close();
  activeProofPage = undefined;

  const filePreview = await openFilePreview(browser);
  activeProofPage = filePreview;
  await proveFilePreview(filePreview.page);
  await assertNoPageErrors(filePreview);
  await filePreview.context.close();
  activeProofPage = undefined;

  const noScriptHome = await openNoScriptHome(browser);
  activeProofPage = noScriptHome;
  await proveNoScriptHome(noScriptHome.page);
  await noScriptHome.context.close();
  activeProofPage = undefined;
} catch (error) {
  failure = error;
  proof.failure = {
    message: error instanceof Error ? error.message : String(error),
    activePage: activeProofPage?.name,
  };
  if (activeProofPage?.page && !activeProofPage.page.isClosed()) {
    try {
      await screenshot(activeProofPage.page, "features-failure.png");
    } catch (screenshotError) {
      proof.failure.screenshotError = screenshotError instanceof Error ? screenshotError.message : String(screenshotError);
    }
  }
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
  proof.assertionSummary = {
    total: proof.assertions.length,
    passed: proof.assertions.filter((assertion) => assertion.ok).length,
  };
  writeProofArtifacts();
}

if (failure) {
  throw failure;
}

function contentType(file) {
  if (file.endsWith(".html")) return "text/html; charset=utf-8";
  if (file.endsWith(".css")) return "text/css; charset=utf-8";
  if (file.endsWith(".js")) return "text/javascript; charset=utf-8";
  if (file.endsWith(".svg")) return "image/svg+xml";
  if (file.endsWith(".png")) return "image/png";
  if (file.endsWith(".webp")) return "image/webp";
  return "application/octet-stream";
}

async function listen(server, port, requirePort) {
  try {
    return await listenOnce(server, port);
  } catch (error) {
    if (error?.code !== "EADDRINUSE" || requirePort) throw error;
    return await listenOnce(server, 0);
  }
}

function listenOnce(server, port) {
  return new Promise((resolve, reject) => {
    const cleanup = () => {
      server.off("error", onError);
      server.off("listening", onListening);
    };
    const onError = (error) => {
      cleanup();
      reject(error);
    };
    const onListening = () => {
      cleanup();
      const address = server.address();
      resolve(`http://127.0.0.1:${address.port}`);
    };
    server.once("error", onError);
    server.once("listening", onListening);
    server.listen(port, "127.0.0.1");
  });
}

async function openProofPage(browser, options) {
  const context = await browser.newContext({
    colorScheme: options.colorScheme,
    deviceScaleFactor: 1,
    isMobile: Boolean(options.isMobile),
    locale: "en-US",
    reducedMotion: "reduce",
    timezoneId: "UTC",
    viewport: options.viewport,
  });
  await context.route("**/*", (route) => {
    const url = route.request().url();
    if (url.startsWith(baseURL) || url.startsWith("data:")) return route.continue();
    return route.abort("blockedbyclient");
  });

  const page = await context.newPage();
  const errors = [];
  page.setDefaultTimeout(7000);
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    const text = message.text();
    if (message.type() === "error" && !text.includes("ERR_BLOCKED_BY_CLIENT")) errors.push(text);
  });

  await page.goto(proof.url, { waitUntil: "domcontentloaded" });
  await page.addStyleTag({
    content:
      "*,*:before,*:after{animation:none!important;transition:none!important;caret-color:transparent!important}html{scroll-behavior:auto!important}",
  });
  await page.waitForFunction(() => {
    return document.querySelector("[data-feature-explorer]") && document.querySelectorAll("[data-fx-card]").length > 0;
  });
  await page.evaluate(async () => {
    if (document.fonts?.ready) await document.fonts.ready;
  });
  await page.waitForTimeout(50);

  return { context, page, errors, name: options.name };
}

async function openFilePreview(browser) {
  const context = await browser.newContext({ colorScheme: "light", viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  const errors = [];
  page.setDefaultTimeout(7000);
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(pathToFileURL(featuresPage).href, { waitUntil: "domcontentloaded" });
  await page.locator("[data-feature-explorer]").waitFor({ state: "visible" });
  return { context, page, errors, name: "direct file preview" };
}

async function openNoScriptHome(browser) {
  const context = await browser.newContext({
    colorScheme: "light",
    javaScriptEnabled: false,
    reducedMotion: "reduce",
    viewport: { width: 1280, height: 900 },
  });
  await context.route("**/*", (route) => {
    const url = route.request().url();
    if (url.startsWith(baseURL) || url.startsWith("data:")) return route.continue();
    return route.abort("blockedbyclient");
  });
  const page = await context.newPage();
  page.setDefaultTimeout(7000);
  await page.goto(proof.homeUrl, { waitUntil: "domcontentloaded" });
  await page.locator('[data-home-job-radio][value="fast-feedback"]').waitFor({ state: "attached" });
  return { context, page, errors: [], name: "no-script homepage" };
}

async function openHome(page) {
  await page.goto(proof.homeUrl, { waitUntil: "domcontentloaded" });
  await page.addStyleTag({
    content:
      "*,*:before,*:after{animation:none!important;transition:none!important;caret-color:transparent!important}html{scroll-behavior:auto!important}",
  });
  await page.locator(".hero-home h1").waitFor({ state: "visible" });
  await page.waitForFunction(() => document.querySelectorAll("[data-home-job-radio]").length === 6);
  await page.evaluate(async () => {
    if (document.fonts?.ready) await document.fonts.ready;
  });
  await page.waitForTimeout(50);
}

async function assertHomeShell(page, name) {
  await assertText(page, ".hero-home h1", /Run Your Code in\s+the Right Box\./, `${name} homepage headline is intact`);
  await assertVisible(page, ".home-capability-note", `${name} nested-capability boundary is visible`);
  const layout = await page.evaluate(() => ({
    innerWidth: window.innerWidth,
    scrollWidth: Math.max(document.documentElement.scrollWidth, document.body.scrollWidth),
    jobChoices: document.querySelectorAll("[data-home-job-radio]").length,
    jobResults: document.querySelectorAll("[data-home-job-result]").length,
    visibleJobResults: [...document.querySelectorAll("[data-home-job-result]")]
      .filter((result) => getComputedStyle(result).display !== "none")
      .map((result) => result.getAttribute("data-home-job-result")),
    selectedJob: document.querySelector("[data-home-job-radio]:checked")?.value || "",
    minimumJobChoiceHeight: Math.min(
      ...[...document.querySelectorAll(".home-job-option label")].map((label) => label.getBoundingClientRect().height),
    ),
    visibleFinderCopyButtons: [...document.querySelectorAll(".home-job-command .copy")]
      .filter((button) => button.getClientRects().length > 0 && getComputedStyle(button).opacity !== "0")
      .map((button) => button.getBoundingClientRect().height),
    controlledJobResults: [...document.querySelectorAll("[data-home-job-radio]")]
      .filter((radio) => document.getElementById(radio.getAttribute("aria-controls"))).length,
    finderDisclaimer: document.querySelector(".home-job-disclaimer")?.textContent.replace(/\s+/g, " ").trim() || "",
    capabilityText: document.querySelector(".home-capability-note")?.textContent.replace(/\s+/g, " ").trim() || "",
    example: document.querySelector(".home-console pre")?.textContent.trim() || "",
    registeredProviders: document.querySelector(".home-facts li:last-child")?.textContent.trim() || "",
  }));
  proof.interactions.home ||= {};
  proof.interactions.home[name] = layout;
  assert(layout.scrollWidth <= layout.innerWidth + 1, `${name} homepage has no horizontal overflow`, layout);
  assert(layout.jobChoices === 6, `${name} homepage exposes six curated job choices`, layout);
  assert(layout.jobResults === 6, `${name} homepage provides one route per job`, layout);
  assert(layout.visibleJobResults.length === 1, `${name} homepage exposes exactly one job result`, layout);
  assert(layout.visibleJobResults[0] === layout.selectedJob, `${name} selected job controls the visible result`, layout);
  assert(layout.minimumJobChoiceHeight >= 44, `${name} job choices meet the minimum touch target`, layout);
  assert(
    layout.visibleFinderCopyButtons.length === 1 && layout.visibleFinderCopyButtons[0] >= 44,
    `${name} finder copy control remains discoverable and meets the minimum touch target`,
    layout,
  );
  assert(layout.controlledJobResults === 6, `${name} job choices identify their result panels`, layout);
  assert(layout.finderDisclaimer.includes("Built-in guidance, not a live provider check."), `${name} homepage states the recommendation boundary`, layout);
  assert(layout.capabilityText.includes("There is no generic nested mode."), `${name} homepage states the nested boundary`, layout);
  assert(layout.example.includes("--provider local-container"), `${name} homepage example names its provider`, layout);
  assert(/^\d+ registered providers$/.test(layout.registeredProviders), `${name} homepage provider count is explicit`, layout);
}

async function proveHomeJobFinder(page) {
  await page.goto(`${proof.homeUrl}?job=not-a-route`, { waitUntil: "domcontentloaded" });
  await page.locator('[data-home-job-radio][value="fast-feedback"]').waitFor({ state: "attached" });
  let state = await page.evaluate(() => ({
    job: new URL(location.href).searchParams.get("job"),
    checked: document.querySelector("[data-home-job-radio]:checked")?.value,
  }));
  assert(state.job === null, "job finder removes an invalid deep-link route", state);
  assert(state.checked === "fast-feedback", "invalid job route falls back to the default", state);

  await page.locator('label[for="home-job-gpu"]').click();
  await page.waitForFunction(() => new URL(location.href).searchParams.get("job") === "gpu");
  state = await page.evaluate(() => ({
    job: new URL(location.href).searchParams.get("job"),
    checked: document.querySelector("[data-home-job-radio]:checked")?.value,
    visible: [...document.querySelectorAll("[data-home-job-result]")]
      .filter((result) => getComputedStyle(result).display !== "none")
      .map((result) => result.getAttribute("data-home-job-result")),
    command: document.querySelector('[data-home-job-result="gpu"] code')?.textContent || "",
    copyText: document.querySelector('[data-home-job-result="gpu"] [data-copy-text]')?.dataset.copyText || "",
  }));
  assert(state.job === "gpu", "job finder writes shareable URL state", state);
  assert(state.checked === "gpu", "job finder selects the requested GPU route", state);
  assert(state.visible.length === 1 && state.visible[0] === "gpu", "job finder shows only the GPU result", state);
  assert(state.command.includes("crabbox providers recommend gpu"), "GPU route exposes the canonical recommendation", state);
  assert(state.copyText === "crabbox providers recommend gpu", "GPU route copies only the runnable recommendation", state);
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"], { origin: new URL(baseURL).origin });
  await page.locator('[data-home-job-result="gpu"] .copy').click();
  await page.locator('[data-home-job-result="gpu"] .copy').getByText("Copied").waitFor();
  const copiedCommand = await page.evaluate(() => navigator.clipboard.readText());
  assert(copiedCommand === "crabbox providers recommend gpu", "finder clipboard contains the exact runnable recommendation", {
    copiedCommand,
  });

  await page.reload({ waitUntil: "domcontentloaded" });
  await page.locator('[data-home-job-radio][value="gpu"]').waitFor({ state: "attached" });
  state = await page.evaluate(() => ({
    checked: document.querySelector("[data-home-job-radio]:checked")?.value,
    visible: [...document.querySelectorAll("[data-home-job-result]")]
      .filter((result) => getComputedStyle(result).display !== "none")
      .map((result) => result.getAttribute("data-home-job-result")),
  }));
  assert(state.checked === "gpu", "job finder restores a deep-linked route", state);
  assert(state.visible.length === 1 && state.visible[0] === "gpu", "deep-linked route restores the matching result", state);

  const gpuRadio = page.locator('[data-home-job-radio][value="gpu"]');
  await gpuRadio.focus();
  await gpuRadio.press("ArrowLeft");
  await page.waitForFunction(() => new URL(location.href).searchParams.get("job") === "fanout-testing");
  state = await page.evaluate(() => ({
    job: new URL(location.href).searchParams.get("job"),
    checked: document.querySelector("[data-home-job-radio]:checked")?.value,
  }));
  assert(state.job === "fanout-testing" && state.checked === "fanout-testing", "native radio keyboard navigation updates the route", state);
}

async function proveFilePreview(page) {
  const initialURL = page.url();
  await page.locator("#fx-search").fill("desktop");
  const visibleCards = await page.locator("[data-fx-card]:visible").count();
  assert(visibleCards > 0, "direct file preview search remains interactive", { visibleCards });
  assert(page.url() === initialURL, "direct file preview avoids unsupported History API writes", { url: page.url() });

  await page.goto(pathToFileURL(homePage).href, { waitUntil: "domcontentloaded" });
  await page.locator('label[for="home-job-gpu"]').click();
  const fileJobState = await page.evaluate(() => ({
    url: location.href,
    checked: document.querySelector("[data-home-job-radio]:checked")?.value,
    visible: [...document.querySelectorAll("[data-home-job-result]")]
      .filter((result) => getComputedStyle(result).display !== "none")
      .map((result) => result.getAttribute("data-home-job-result")),
  }));
  assert(fileJobState.checked === "gpu", "direct file preview job finder remains interactive", fileJobState);
  assert(fileJobState.visible.length === 1 && fileJobState.visible[0] === "gpu", "direct file preview updates the job result", fileJobState);
  assert(!new URL(fileJobState.url).searchParams.has("job"), "direct file preview job finder avoids unsupported History API writes", fileJobState);
}

async function proveNoScriptHome(page) {
  await page.locator('[data-home-job-radio][value="gpu"]').focus();
  await page.keyboard.press("Space");
  let state = await noScriptJobState(page);
  assert(state.checked === "gpu", "no-script job finder selects a route with native controls", state);
  assert(state.visible.length === 1 && state.visible[0] === "gpu", "no-script job finder shows the selected result", state);

  await page.keyboard.press("ArrowLeft");
  state = await noScriptJobState(page);
  assert(state.checked === "fanout-testing", "no-script job finder supports native keyboard navigation", state);
  assert(state.visible.length === 1 && state.visible[0] === "fanout-testing", "no-script keyboard navigation updates the result", state);
  proof.interactions.noScriptHome = state;
}

async function noScriptJobState(page) {
  return page.evaluate(() => ({
    checked: document.querySelector("[data-home-job-radio]:checked")?.value || "",
    visible: [...document.querySelectorAll("[data-home-job-result]")]
      .filter((result) => getComputedStyle(result).display !== "none")
      .map((result) => result.getAttribute("data-home-job-result")),
  }));
}

async function assertFeatureShell(page) {
  await assertVisible(page, ".fx-hero h1", "hero headline renders");
  await assertText(page, ".fx-hero h1", /Build locally\.\s*Run remotely\.\s*Prove every result\./, "hero headline copy is intact");
  await assertVisible(page, "[data-feature-explorer]", "feature explorer result region renders");
  await assertVisible(page, "[data-fx-filter-bar]", "feature explorer controls render");

  const state = await featureState(page);
  proof.interactions.initial = {
    totalCards: state.totalCards,
    totalSections: state.sections.length,
    countText: state.countText,
    pressedFilters: state.filters.filter((filter) => filter.pressed === "true").map((filter) => filter.area),
  };
  assert(state.totalCards >= 20, "feature explorer exposes the generated capability cards", {
    totalCards: state.totalCards,
  });
  assert(state.sections.length >= 4, "feature explorer exposes capability area sections", {
    sections: state.sections.map((section) => section.area),
  });
  assert(state.visibleCards === state.totalCards, "initial explorer state shows every card", {
    visibleCards: state.visibleCards,
    totalCards: state.totalCards,
  });
  assert(state.countText === `${state.totalCards} capabilities`, "initial count matches generated card count", {
    countText: state.countText,
    totalCards: state.totalCards,
  });
  assert(
    state.filters.filter((filter) => filter.pressed === "true").map((filter) => filter.area).join(",") === "all",
    "initial filter pressed state is all only",
    { filters: state.filters },
  );
}

async function assertHandoff(page) {
  await assertVisible(page, ".fx-boundary aside a[href='../providers/index.html']", "provider reference handoff is visible");
  await assertText(page, ".fx-boundary aside a[href='../providers/index.html']", /Choose a provider/, "provider handoff label is visible");
  await assertVisible(page, ".fx-boundary aside a[href='../commands/index.html']", "command reference handoff is visible");
  await assertText(page, ".fx-boundary aside a[href='../commands/index.html']", /Open command reference/, "command handoff label is visible");

  const layout = await page.locator(".fx-boundary aside").evaluate((aside) => {
    const visible = (element) => {
      if (!element) return false;
      const style = getComputedStyle(element);
      return element.getClientRects().length > 0 && style.visibility !== "hidden" && style.display !== "none";
    };
    return [...aside.querySelectorAll("a")].map((link) => {
      const rect = link.getBoundingClientRect();
      const text = link.querySelector("div");
      const textRect = text?.getBoundingClientRect();
      const iconRect = link.querySelector("span")?.getBoundingClientRect();
      const arrowRect = link.querySelector("i")?.getBoundingClientRect();
      return {
        href: link.getAttribute("href"),
        label: link.querySelector("strong")?.textContent.trim() || "",
        body: link.querySelector("p")?.textContent.trim() || "",
        visible: visible(link),
        cardWidth: Math.round(rect.width),
        cardHeight: Math.round(rect.height),
        textWidth: Math.round(textRect?.width || 0),
        textHeight: Math.round(textRect?.height || 0),
        noHorizontalOverflow: Boolean(text && text.scrollWidth <= text.clientWidth + 1 && link.scrollWidth <= link.clientWidth + 1),
        iconBeforeText: Boolean(iconRect && textRect && iconRect.right <= textRect.left),
        arrowAfterText: Boolean(arrowRect && textRect && textRect.right <= arrowRect.left + 1),
      };
    });
  });

  proof.interactions.handoff = layout;
  const labels = layout.map((item) => item.label);
  assert(layout.length === 2, "handoff renders exactly the provider and command cards", { labels });
  assert(labels.includes("Choose a provider") && labels.includes("Open command reference"), "handoff card labels identify provider and command references", {
    labels,
  });
  assert(
    layout.every((item) => item.visible && item.cardWidth >= 280 && item.textWidth >= 180 && item.textHeight > 40),
    "handoff cards keep readable text columns",
    { layout },
  );
  assert(layout.every((item) => item.noHorizontalOverflow), "handoff card text does not horizontally overflow", { layout });
  assert(layout.every((item) => item.iconBeforeText && item.arrowAfterText), "handoff card icon, text, and arrow stay in order", {
    layout,
  });
}

async function proveSlashSearch(page) {
  await resetExplorer(page);
  await page.locator("body").click({ position: { x: 10, y: 10 } });
  await page.keyboard.press("/");
  await page.waitForFunction(() => document.activeElement?.id === "fx-search");
  const activeElement = await page.evaluate(() => document.activeElement?.id || "");
  assert(activeElement === "fx-search", "slash focuses feature search", { activeElement });

  const query = "desktop";
  await page.locator("#fx-search").fill(query);
  await waitForSearch(page, query);
  const state = await featureState(page);
  const expected = state.cards.filter((card) => card.search.includes(query));
  const visible = state.cards.filter((card) => !card.hidden);
  proof.interactions.search = {
    query,
    visibleCards: state.visibleCards,
    totalCards: state.totalCards,
    countText: state.countText,
    urlSearch: state.urlSearch,
    visibleTitles: visible.map((card) => card.title),
  };

  assert(expected.length > 0, "desktop search has generated matching cards", { query });
  assert(state.visibleCards === expected.length, "search result count matches matching cards", {
    visibleCards: state.visibleCards,
    expectedCards: expected.length,
  });
  assert(state.visibleCards > 0 && state.visibleCards < state.totalCards, "search narrows visible cards", {
    visibleCards: state.visibleCards,
    totalCards: state.totalCards,
  });
  assert(visible.every((card) => card.search.includes(query)), "search leaves only cards matching the query visible", {
    query,
    visibleTitles: visible.map((card) => card.title),
  });
  assert(state.countText === `${state.visibleCards} capabilities`, "search updates the capability count", {
    countText: state.countText,
    visibleCards: state.visibleCards,
  });
  assert(state.clearVisible, "search exposes the clear control", { clearVisible: state.clearVisible });
  assert(new URLSearchParams(state.urlSearch).get("q") === query, "search query is reflected in the URL", {
    urlSearch: state.urlSearch,
  });
}

async function proveFilter(page) {
  await resetExplorer(page);
  const area = "sync-execution-and-evidence";
  await page.locator(`[data-fx-filter='${area}']`).click();
  await page.waitForFunction((selectedArea) => {
    return [...document.querySelectorAll("[data-fx-card]")].every((card) => card.hidden === (card.dataset.fxArea !== selectedArea));
  }, area);

  const state = await featureState(page);
  const visible = state.cards.filter((card) => !card.hidden);
  const selected = state.cards.filter((card) => card.area === area);
  proof.interactions.filter = {
    area,
    visibleCards: state.visibleCards,
    selectedAreaCards: selected.length,
    countText: state.countText,
    urlSearch: state.urlSearch,
    pressedFilters: state.filters.filter((filter) => filter.pressed === "true").map((filter) => filter.area),
    hiddenSections: state.sections.filter((section) => section.hidden).map((section) => section.area),
  };

  assert(
    state.filters.find((filter) => filter.area === area)?.pressed === "true" &&
      state.filters.filter((filter) => filter.area !== area).every((filter) => filter.pressed === "false"),
    "filter button has exclusive pressed state",
    { filters: state.filters },
  );
  assert(visible.length === selected.length && visible.every((card) => card.area === area), "filter hides cards from other capability areas", {
    visibleCards: visible.map((card) => `${card.area}:${card.title}`),
    selectedAreaCards: selected.length,
  });
  assert(
    state.sections.find((section) => section.area === area)?.hidden === false &&
      state.sections.filter((section) => section.area !== area).every((section) => section.hidden),
    "filter hides non-selected capability sections",
    { sections: state.sections },
  );
  assert(state.countText === `${selected.length} capabilities`, "filter count matches selected area", {
    countText: state.countText,
    selectedAreaCards: selected.length,
  });
  assert(new URLSearchParams(state.urlSearch).get("area") === area, "selected filter is reflected in the URL", {
    urlSearch: state.urlSearch,
  });
}

async function proveDeepLinkedState(page) {
  const query = "evidence";
  const area = "sync-execution-and-evidence";
  await page.goto(`${proof.url}?q=${encodeURIComponent(query)}&area=${encodeURIComponent(area)}`, { waitUntil: "domcontentloaded" });
  await page.waitForFunction((expected) => {
    const input = document.querySelector("#fx-search");
    const selected = document.querySelector(`[data-fx-filter='${expected.area}']`);
    const visible = [...document.querySelectorAll("[data-fx-card]")].filter((card) => !card.hidden);
    return input?.value === expected.query &&
      selected?.getAttribute("aria-pressed") === "true" &&
      visible.length > 0 &&
      visible.every((card) => card.dataset.fxArea === expected.area && (card.dataset.fxSearch || "").includes(expected.query));
  }, { query, area });

  const state = await featureState(page);
  proof.interactions.deepLink = {
    query,
    area,
    visibleCards: state.visibleCards,
    countText: state.countText,
    urlSearch: state.urlSearch,
  };
  assert(state.searchValue === query, "deep link restores feature search query", { searchValue: state.searchValue });
  assert(state.filters.find((filter) => filter.area === area)?.pressed === "true", "deep link restores selected feature area", {
    filters: state.filters,
  });
  assert(
    state.visibleCards > 0 &&
      state.cards.filter((card) => !card.hidden).every((card) => card.area === area && card.search.includes(query)),
    "deep link applies search and area filters together",
    { visibleCards: state.cards.filter((card) => !card.hidden).map((card) => `${card.area}:${card.title}`) },
  );
}

async function proveEscapeClears(page) {
  await resetExplorer(page);
  const query = "cache";
  await page.locator("#fx-search").fill(query);
  await waitForSearch(page, query);
  const narrowed = await featureState(page);
  assert(narrowed.visibleCards > 0 && narrowed.visibleCards < narrowed.totalCards, "Escape proof starts from a narrowed search", {
    query,
    visibleCards: narrowed.visibleCards,
    totalCards: narrowed.totalCards,
  });

  await page.keyboard.press("Escape");
  await page.waitForFunction(() => {
    const input = document.querySelector("#fx-search");
    const cards = [...document.querySelectorAll("[data-fx-card]")];
    return input?.value === "" && cards.every((card) => !card.hidden);
  });

  const state = await featureState(page);
  proof.interactions.escape = {
    query,
    valueAfterEscape: state.searchValue,
    visibleCards: state.visibleCards,
    totalCards: state.totalCards,
    countText: state.countText,
    clearVisible: state.clearVisible,
    urlSearch: state.urlSearch,
  };

  assert(state.searchValue === "", "Escape clears active search text", { searchValue: state.searchValue });
  assert(state.visibleCards === state.totalCards, "Escape restores all capability cards", {
    visibleCards: state.visibleCards,
    totalCards: state.totalCards,
  });
  assert(state.countText === `${state.totalCards} capabilities`, "Escape restores the full capability count", {
    countText: state.countText,
    totalCards: state.totalCards,
  });
  assert(!state.clearVisible, "Escape hides the clear control", { clearVisible: state.clearVisible });
  assert(!new URLSearchParams(state.urlSearch).has("q"), "Escape removes search query from the URL", {
    urlSearch: state.urlSearch,
  });
}

async function proveEmptyResult(page) {
  await resetExplorer(page);
  const query = "zzzz-no-such-capability";
  await page.locator("#fx-search").fill(query);
  await page.waitForFunction(() => {
    const cards = [...document.querySelectorAll("[data-fx-card]")];
    const empty = document.querySelector("[data-fx-empty]");
    return cards.length > 0 && cards.every((card) => card.hidden) && empty && !empty.hidden;
  });

  const state = await featureState(page);
  proof.interactions.empty = {
    query,
    visibleCards: state.visibleCards,
    totalCards: state.totalCards,
    countText: state.countText,
    emptyVisible: state.emptyVisible,
    clearVisible: state.clearVisible,
    hiddenSections: state.sections.filter((section) => section.hidden).map((section) => section.area),
  };

  assert(state.visibleCards === 0, "empty search hides every card", {
    visibleCards: state.visibleCards,
    totalCards: state.totalCards,
  });
  assert(state.sections.every((section) => section.hidden), "empty search hides every capability section", {
    sections: state.sections,
  });
  assert(state.emptyVisible, "empty state appears for no results", { emptyVisible: state.emptyVisible });
  assert(state.countText === "0 capabilities", "empty search reports zero capabilities", { countText: state.countText });
  assert(state.clearVisible, "empty search still exposes the clear control", { clearVisible: state.clearVisible });
}

async function assertMobileLayout(page) {
  await assertFeatureShell(page);
  const layout = await page.evaluate(() => {
    const boundaryCards = [...document.querySelectorAll(".fx-boundary aside a")].map((link) => {
      const rect = link.getBoundingClientRect();
      const text = link.querySelector("div")?.getBoundingClientRect();
      return {
        label: link.querySelector("strong")?.textContent.trim() || "",
        cardWidth: Math.round(rect.width),
        textWidth: Math.round(text?.width || 0),
      };
    });
    return {
      innerWidth: window.innerWidth,
      scrollWidth: Math.max(document.documentElement.scrollWidth, document.body.scrollWidth),
      filterPosition: getComputedStyle(document.querySelector(".fx-filter")).position,
      gridColumns: getComputedStyle(document.querySelector(".fx-grid")).gridTemplateColumns,
      boundaryCards,
    };
  });
  proof.interactions.mobile = layout;
  assert(layout.scrollWidth <= layout.innerWidth + 1, "mobile layout has no horizontal overflow", layout);
  assert(layout.filterPosition === "static", "mobile filter is not sticky over content", layout);
  assert(layout.boundaryCards.length === 2 && layout.boundaryCards.every((card) => card.cardWidth >= 300 && card.textWidth >= 180), "mobile handoff cards remain readable", {
    boundaryCards: layout.boundaryCards,
  });
}

async function assertTheme(page, expected) {
  const actual = await page.evaluate(() => document.documentElement.dataset.theme || "");
  assert(actual === expected, `${expected} color scheme applies to the page`, { expected, actual });
}

async function resetExplorer(page) {
  await page.locator("[data-fx-filter='all']").click();
  await page.locator("#fx-search").fill("");
  await page.waitForFunction(() => {
    const cards = [...document.querySelectorAll("[data-fx-card]")];
    const sections = [...document.querySelectorAll("[data-fx-section]")];
    return (
      document.querySelector("#fx-search")?.value === "" &&
      cards.length > 0 &&
      cards.every((card) => !card.hidden) &&
      sections.every((section) => !section.hidden) &&
      document.querySelector("[data-fx-filter='all']")?.getAttribute("aria-pressed") === "true"
    );
  });
}

async function waitForSearch(page, query) {
  await page.waitForFunction((value) => {
    const input = document.querySelector("#fx-search");
    const cards = [...document.querySelectorAll("[data-fx-card]")];
    return input?.value === value && cards.every((card) => card.hidden === !(card.dataset.fxSearch || "").includes(value));
  }, query);
}

async function featureState(page) {
  return await page.evaluate(() => {
    const visible = (element) => {
      if (!element) return false;
      const style = getComputedStyle(element);
      return element.getClientRects().length > 0 && style.visibility !== "hidden" && style.display !== "none";
    };
    const cards = [...document.querySelectorAll("[data-fx-card]")].map((card) => ({
      title: card.querySelector("a")?.textContent.trim() || "",
      area: card.dataset.fxArea || "",
      search: card.dataset.fxSearch || "",
      hidden: card.hidden,
    }));
    const sections = [...document.querySelectorAll("[data-fx-section]")].map((section) => ({
      area: section.dataset.fxArea || "",
      hidden: section.hidden,
      visibleCards: [...section.querySelectorAll("[data-fx-card]")].filter((card) => !card.hidden).length,
    }));
    const filters = [...document.querySelectorAll("[data-fx-filter]")].map((filter) => ({
      area: filter.dataset.fxFilter || "",
      label: filter.textContent.trim(),
      pressed: filter.getAttribute("aria-pressed"),
    }));
    return {
      cards,
      sections,
      filters,
      totalCards: cards.length,
      visibleCards: cards.filter((card) => !card.hidden).length,
      countText: document.querySelector("[data-fx-count]")?.textContent.trim() || "",
      searchValue: document.querySelector("#fx-search")?.value || "",
      emptyVisible: visible(document.querySelector("[data-fx-empty]")),
      clearVisible: visible(document.querySelector("[data-fx-clear]")),
      urlSearch: location.search,
    };
  });
}

async function screenshot(page, file) {
  const target = path.join(outDir, file);
  await page.screenshot({ path: target, fullPage: true, animations: "disabled", caret: "hide", scale: "css" });
  const buffer = fs.readFileSync(target);
  const dimensions = pngDimensions(buffer);
  const artifact = {
    file,
    sha256: sha256(buffer),
    bytes: buffer.length,
    width: dimensions.width,
    height: dimensions.height,
  };
  proof.screenshots.push(artifact);
  assert(artifact.bytes > 4096 && artifact.width > 0 && artifact.height > 0, `screenshot captured: ${file}`, artifact);
}

async function assertVisible(page, selector, label) {
  try {
    await page.locator(selector).first().waitFor({ state: "visible" });
    record(label, true, { selector });
  } catch (error) {
    record(label, false, { selector, error: error.message });
  }
}

async function assertText(page, selector, matcher, label) {
  const text = (await page.locator(selector).first().textContent())?.replace(/\s+/g, " ").trim() || "";
  assert(matcher.test(text), label, { selector, text });
}

async function assertNoPageErrors(result) {
  assert(result.errors.length === 0, `${result.name} page has no JavaScript console errors`, { errors: result.errors });
}

function assert(ok, label, details = {}) {
  record(label, Boolean(ok), details);
}

function record(label, ok, details = {}) {
  const assertion = Object.keys(details).length ? { label, ok, details } : { label, ok };
  proof.assertions.push(assertion);
  if (!ok) {
    const suffix = Object.keys(details).length ? ` ${JSON.stringify(details)}` : "";
    throw new Error(`proof assertion failed: ${label}${suffix}`);
  }
}

function writeProofArtifacts() {
  const proofFile = path.join(outDir, "interaction-proof.json");
  fs.writeFileSync(proofFile, `${JSON.stringify(proof, null, 2)}\n`, "utf8");

  const artifactFiles = ["interaction-proof.json", ...proof.screenshots.map((item) => item.file)].sort();
  const sums = artifactFiles
    .map((file) => {
      const buffer = fs.readFileSync(path.join(outDir, file));
      return `${sha256(buffer)}  ${file}`;
    })
    .join("\n");
  fs.writeFileSync(path.join(outDir, "SHA256SUMS"), `${sums}\n`, "utf8");
}

function sha256(buffer) {
  return crypto.createHash("sha256").update(buffer).digest("hex");
}

function pngDimensions(buffer) {
  const pngSignature = "89504e470d0a1a0a";
  if (buffer.subarray(0, 8).toString("hex") !== pngSignature || buffer.subarray(12, 16).toString("ascii") !== "IHDR") {
    throw new Error("screenshot is not a PNG");
  }
  return {
    width: buffer.readUInt32BE(16),
    height: buffer.readUInt32BE(20),
  };
}
