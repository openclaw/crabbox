import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { expect, it } from "vitest";

import { cloudInit } from "../src/bootstrap";
import { leaseConfig } from "../src/config";

function installedBootstrapFile(path: string): string {
  const document = cloudInit(
    leaseConfig({ provider: "aws", desktop: true, sshPublicKey: "ssh-ed25519 fixture" }),
  );
  const files = document.split(/^  - path: /m).slice(1);
  const matches = files.filter((file) => file.startsWith(`${path}\n`));
  expect(matches, `installed bootstrap file ${path}`).toHaveLength(1);
  const lines = matches[0]!.split("\n");
  const start = lines.indexOf("    content: |");
  expect(start).toBeGreaterThan(0);
  const content: string[] = [];
  for (const line of lines.slice(start + 1)) {
    if (line !== "" && !line.startsWith("      ")) break;
    content.push(line.slice(6));
  }
  return content.join("\n");
}

it("retains the released reset name as an alias of the XFCE service", () => {
  const document = cloudInit(
    leaseConfig({ provider: "aws", desktop: true, sshPublicKey: "ssh-ed25519 fixture" }),
  );
  const paths = [...document.matchAll(/^  - path: (.+)$/gm)].map((match) => match[1]);
  expect(paths).not.toContain("/etc/systemd/system/crabbox-desktop-session.service");
  const desktop = installedBootstrapFile("/etc/systemd/system/crabbox-desktop.service");
  const values = new Map<string, string>();
  let section = "";
  for (const raw of desktop.split("\n")) {
    const line = raw.trim();
    if (line.startsWith("[") && line.endsWith("]")) {
      section = line.slice(1, -1);
    } else {
      const separator = line.indexOf("=");
      if (separator >= 0)
        values.set(`${section}/${line.slice(0, separator)}`, line.slice(separator + 1));
    }
  }
  expect(values.get("Service/ExecStart")).toBe("/usr/bin/startxfce4");
  expect(values.get("Install/Alias")).toBe("crabbox-desktop-session.service");
});

// Keep filesystem effects in a temporary home and replace external desktop
// commands with a bus-sensitive settings sink and a component ownership ledger.
const fixtureCommands = `
getent() { printf 'fixture:x:%s:%s::%s:/bin/bash\\n' "$FIXTURE_UID" "$FIXTURE_UID" "$FIXTURE_HOME"; }
id() {
  case "$1" in
    -u) printf '%s\\n' "$FIXTURE_UID" ;;
    -un|-nu) printf 'fixture\\n' ;;
    *) return 97 ;;
  esac
}
install() {
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -d) shift ;;
      -m|-o|-g) shift 2 ;;
      *) mkdir -p "$1"; shift ;;
    esac
  done
}
chown() { :; }
pgrep() {
  case " $* " in
    *" xfce4-session "*)
      [ -n "$FIXTURE_SESSION_PIDS" ] || return 1
      printf '%s\\n' "$FIXTURE_SESSION_PIDS" ;;
    *"xfce4-terminal"*"Crabbox Desktop"*) [ "$FIXTURE_PREFERRED_EXISTING" = 1 ] ;;
    *"xterm"*"Crabbox Desktop"*) [ "$FIXTURE_FALLBACK_EXISTING" = 1 ] ;;
    *) return 1 ;;
  esac
}
xfconf-query() {
  printf 'xfconf\\t%s\\t%s\\t%s\\n' "\${DBUS_SESSION_BUS_ADDRESS:-}" "\${DISPLAY:-}" "$*" >> "$FIXTURE_QUERIES"
  [ "\${DBUS_SESSION_BUS_ADDRESS:-}" = "$FIXTURE_EXPECTED_BUS" ]
}
gsettings() {
  printf 'gsettings\\t%s\\t%s\\t%s\\n' "\${DBUS_SESSION_BUS_ADDRESS:-}" "\${DISPLAY:-}" "$*" >> "$FIXTURE_QUERIES"
  [ "\${DBUS_SESSION_BUS_ADDRESS:-}" = "$FIXTURE_EXPECTED_BUS" ]
}
pkill() { printf 'signal %s\\n' "$*" >> "$FIXTURE_COMPONENTS"; }
xfce4-panel() { printf 'panel %s\\n' "$*" >> "$FIXTURE_COMPONENTS"; }
xfdesktop() { printf 'desktop %s\\n' "$*" >> "$FIXTURE_COMPONENTS"; }
xfwm4() { printf 'window-manager %s\\n' "$*" >> "$FIXTURE_COMPONENTS"; }
fixture_terminal() {
  terminal_command="$1"
  shift
  terminal_title=
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --title=*) terminal_title="\${1#*=}" ;;
      --title|-title|-T) shift; terminal_title="$1" ;;
    esac
    shift
  done
  printf '%s\\t%s\\t%s\\t%s\\n' "$terminal_command" "\${DBUS_SESSION_BUS_ADDRESS:-}" "\${DISPLAY:-}" "$terminal_title" >> "$FIXTURE_TERMINALS"
}
xfce4-terminal() { fixture_terminal xfce4-terminal "$@"; }
xterm() { fixture_terminal xterm "$@"; }
command() {
  if [ "$1" = -v ]; then
    case "$2" in
      xfce4-terminal) [ "$FIXTURE_PREFERRED_AVAILABLE" = 1 ] || return 1 ;;
      xterm) [ "$FIXTURE_FALLBACK_AVAILABLE" = 1 ] || return 1 ;;
    esac
  fi
  case "$1" in
    xfce4-terminal|xterm) "$@" ;;
    *) builtin command "$@" ;;
  esac
}
xsetroot() { :; }
sleep() { :; }
export -f getent id install chown pgrep xfconf-query gsettings pkill xfce4-panel xfdesktop xfwm4 fixture_terminal xfce4-terminal xterm command xsetroot sleep
`;

const rows = (file: string) =>
  readFileSync(file, "utf8")
    .trim()
    .split("\n")
    .filter(Boolean)
    .map((line) => line.split("\t"));

function stopFixtureProcessGroup(pid: number) {
  try {
    process.kill(-pid, "SIGKILL");
  } catch (error) {
    if (!(error instanceof Error && "code" in error && error.code === "ESRCH")) throw error;
  }
}

interface TerminalSelection {
  preferredAvailable: boolean;
  preferredExisting: boolean;
  fallbackAvailable: boolean;
  fallbackExisting: boolean;
}

const relocateSessionLibrary = (source: string) =>
  source.replaceAll("/usr/local/lib/crabbox/xfce-session.sh", '"$FIXTURE_SESSION_LIBRARY"');

function themeFixture(terminalSelection?: TerminalSelection) {
  const directory = mkdtempSync(join(tmpdir(), "crabbox-xfce-session-"));
  const script = join(directory, "theme.sh");
  const sessionLibrary = join(directory, "xfce-session.sh");
  const queryPath = join(directory, "queries");
  const componentPath = join(directory, "components");
  const terminalPath = join(directory, "terminals");
  const home = join(directory, "home");
  const sessions: ChildProcess[] = [];
  // Portable terminal-selection cases stub theme and session binding;
  // the Linux case below runs its complete generated script and real /proc reads.
  // Historical helper component logs also stay inside the temporary directory.
  writeFileSync(
    sessionLibrary,
    terminalSelection ? ":\n" : installedBootstrapFile("/usr/local/lib/crabbox/xfce-session.sh"),
  );
  writeFileSync(
    script,
    terminalSelection
      ? "#!/bin/sh\nexit 0\n"
      : relocateSessionLibrary(
          installedBootstrapFile("/usr/local/bin/crabbox-configure-desktop-theme"),
        ).replaceAll("/tmp/crabbox-", "${FIXTURE_ROOT}/crabbox-"),
    { mode: 0o755 },
  );
  function environment(display?: string, callerBus?: string, expectedBus = "") {
    return {
      PATH: "/usr/bin:/bin",
      HOME: home,
      CRABBOX_DESKTOP_USER: "fixture",
      FIXTURE_ROOT: directory,
      FIXTURE_HOME: home,
      FIXTURE_UID: String(process.getuid?.() ?? 1001),
      FIXTURE_SESSION_PIDS: sessions.map((child) => child.pid).join("\n"),
      FIXTURE_EXPECTED_BUS: expectedBus,
      FIXTURE_QUERIES: queryPath,
      FIXTURE_COMPONENTS: componentPath,
      FIXTURE_TERMINALS: terminalPath,
      FIXTURE_THEME_SCRIPT: script,
      FIXTURE_SESSION_LIBRARY: sessionLibrary,
      FIXTURE_PREFERRED_AVAILABLE: terminalSelection?.preferredAvailable === false ? "0" : "1",
      FIXTURE_PREFERRED_EXISTING: terminalSelection?.preferredExisting ? "1" : "0",
      FIXTURE_FALLBACK_AVAILABLE: terminalSelection?.fallbackAvailable === false ? "0" : "1",
      FIXTURE_FALLBACK_EXISTING: terminalSelection?.fallbackExisting ? "1" : "0",
      ...(display ? { DISPLAY: display } : {}),
      ...(callerBus ? { DBUS_SESSION_BUS_ADDRESS: callerBus } : {}),
    };
  }
  function installedAutostart() {
    const desktop = installedBootstrapFile("/etc/xdg/autostart/crabbox-desktop.desktop");
    const entry = new Map(
      desktop
        .split("\n")
        .filter((line) => line.includes("="))
        .map((line): [string, string] => {
          const separator = line.indexOf("=");
          return [line.slice(0, separator), line.slice(separator + 1)];
        }),
    );
    expect(entry.get("Type")).toBe("Application");
    expect(entry.get("OnlyShowIn")?.split(";")).toContain("XFCE");
    expect(entry.get("Hidden")).not.toBe("true");
    const command = entry.get("Exec");
    expect(command).toMatch(/^\/\S+$/);
    const launcher = join(directory, "autostart.sh");
    writeFileSync(
      launcher,
      relocateSessionLibrary(installedBootstrapFile(command!)).replaceAll(
        "/usr/local/bin/crabbox-configure-desktop-theme",
        '"$FIXTURE_THEME_SCRIPT"',
      ),
    );
    return launcher;
  }
  function execute(
    path: string,
    mode: string,
    display?: string,
    callerBus?: string,
    expectedBus = "",
  ) {
    writeFileSync(queryPath, "");
    writeFileSync(componentPath, "");
    writeFileSync(terminalPath, "");
    const result = spawnSync(
      "bash",
      ["-c", fixtureCommands + '\nsource "$1" "$2"\nwait\n', "xfce-fixture", path, mode],
      {
        env: environment(display, callerBus, expectedBus),
        encoding: "utf8",
        timeout: 5000,
      },
    );
    expect(result.error).toBeUndefined();
    return {
      ...result,
      queries: rows(queryPath),
      components: readFileSync(componentPath, "utf8"),
      terminals: rows(terminalPath),
    };
  }
  return {
    home,
    async session(display: string, bus?: string) {
      const child = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)"], {
        env: { DISPLAY: display, ...(bus ? { DBUS_SESSION_BUS_ADDRESS: bus } : {}) },
        stdio: "ignore",
      });
      sessions.push(child);
      await once(child, "spawn");
    },
    run(mode: "light" | "dark", display?: string, callerBus?: string, expectedBus = "") {
      return execute(script, mode, display, callerBus, expectedBus);
    },
    autostart(display: string, callerBus?: string, expectedBus = callerBus ?? "") {
      return execute(installedAutostart(), "", display, callerBus, expectedBus);
    },
    spawnAutostart(guiProgram: string) {
      const gui = join(directory, "gui.mjs");
      writeFileSync(gui, guiProgram);
      return spawn(
        "bash",
        [
          "-c",
          fixtureCommands +
            '\nfixture_terminal() { exec "$FIXTURE_NODE" "$FIXTURE_GUI" "$@"; }\nexport -f fixture_terminal\nsource "$1"\n',
          "xfce-transport-fixture",
          installedAutostart(),
        ],
        {
          env: { ...environment(":99"), FIXTURE_NODE: process.execPath, FIXTURE_GUI: gui },
          // This group contains only the launcher and its inert GUI substitute.
          detached: true,
          stdio: ["pipe", "pipe", "pipe", "pipe"],
        },
      );
    },
    async close() {
      await Promise.all(
        sessions.map(async (child) => {
          if (child.exitCode !== null || child.signalCode !== null) return;
          const exited = once(child, "exit");
          child.kill();
          await exited;
        }),
      );
      rmSync(directory, { recursive: true, force: true });
    },
  };
}

function xfconfValue(queries: string[][], channel: string, property: string) {
  let value: string | undefined;
  for (const [kind, , , args] of queries) {
    if (kind !== "xfconf" || !args) continue;
    const words = args.split(" ");
    const option = (...flags: string[]) => {
      const index = words.findIndex((word) => flags.includes(word));
      return index >= 0 ? words[index + 1] : undefined;
    };
    if (option("-c", "--channel") === channel && option("-p", "--property") === property) {
      value = option("-s", "--set");
    }
  }
  return value;
}

it.skipIf(process.platform === "win32")(
  "seeds an offline XFCE theme without starting a settings or desktop owner",
  async () => {
    const fixture = themeFixture();
    try {
      const result = fixture.run("light");
      expect({ status: result.status, stderr: result.stderr }).toEqual({ status: 0, stderr: "" });
      expect(readFileSync(join(fixture.home, ".config/crabbox/desktop-theme"), "utf8").trim()).toBe(
        "light",
      );
      expect(readFileSync(join(fixture.home, ".config/gtk-3.0/settings.ini"), "utf8")).toContain(
        "gtk-application-prefer-dark-theme=0",
      );
      expect(result.queries).toEqual([]);
      expect(result.components).toBe("");
    } finally {
      await fixture.close();
    }
  },
);

it.skipIf(process.platform === "win32")(
  "rejects a live theme request without an XFCE session instead of using the caller bus",
  async () => {
    const fixture = themeFixture();
    try {
      const result = fixture.run("light", ":99", "unix:abstract=unrelated-caller");
      expect(result.status).not.toBe(0);
      expect(result.queries).toEqual([]);
      expect(result.components).toBe("");
    } finally {
      await fixture.close();
    }
  },
);

it.skipIf(process.platform !== "linux")(
  "updates the matching XFCE session through its bus and preserves component ownership",
  async () => {
    const fixture = themeFixture();
    const bus = "unix:abstract=crabbox-fixture-target";
    try {
      const channels = join(fixture.home, ".config/xfce4/xfconf/xfce-perchannel-xml");
      mkdirSync(channels, { recursive: true });
      const xsettings = join(channels, "xsettings.xml");
      const existingSettings =
        '<?xml version="1.0" encoding="UTF-8"?>\n<channel name="xsettings" version="1.0"><property name="Net" type="empty"><property name="DoubleClickTime" type="int" value="350"/></property></channel>\n';
      writeFileSync(xsettings, existingSettings);
      await fixture.session(":77", "unix:abstract=crabbox-fixture-other-display");
      await fixture.session(":99", bus);
      for (const [mode, preferDark, callerBus] of [
        ["light", "false", undefined],
        ["dark", "true", "unix:abstract=unrelated-caller"],
      ] as const) {
        const result = fixture.run(mode, ":99", callerBus, bus);
        expect({ status: result.status, stderr: result.stderr }).toEqual({ status: 0, stderr: "" });
        expect(
          readFileSync(join(fixture.home, ".config/crabbox/desktop-theme"), "utf8").trim(),
        ).toBe(mode);
        expect(result.queries.length).toBeGreaterThan(0);
        for (const [, actualBus, display] of result.queries) {
          expect(actualBus).toBe(bus);
          expect(display).toBe(":99");
        }
        expect(xfconfValue(result.queries, "xsettings", "/Gtk/ApplicationPreferDarkTheme")).toBe(
          preferDark,
        );
        expect(xfconfValue(result.queries, "xfwm4", "/general/use_compositing")).toBe("false");
        expect(readFileSync(xsettings, "utf8")).toBe(existingSettings);
        expect(result.queries).toContainEqual([
          "gsettings",
          bus,
          ":99",
          `set org.gnome.desktop.interface color-scheme prefer-${mode}`,
        ]);
        expect(result.components).toBe("");
      }
    } finally {
      await fixture.close();
    }
  },
);

it.skipIf(process.platform !== "linux").each([
  { sessionDisplay: ":99", callerDisplay: ":99.0" },
  { sessionDisplay: ":99.0", callerDisplay: ":99" },
])(
  "binds XFCE screen-zero alias $sessionDisplay to caller $callerDisplay",
  async ({ sessionDisplay, callerDisplay }) => {
    const fixture = themeFixture();
    const bus = "unix:abstract=crabbox-fixture-screen-zero";
    try {
      await fixture.session(sessionDisplay, bus);
      const result = fixture.autostart(callerDisplay, "unix:abstract=unrelated-caller", bus);
      expect({ status: result.status, stderr: result.stderr }).toEqual({ status: 0, stderr: "" });
      expect(result.terminals).toEqual([["xfce4-terminal", bus, callerDisplay, "Crabbox Desktop"]]);
      expect(result.queries.length).toBeGreaterThan(0);
      for (const [, actualBus, actualDisplay] of result.queries) {
        expect(actualBus).toBe(bus);
        expect(actualDisplay).toBe(callerDisplay);
      }
      expect(result.components).toBe("");
    } finally {
      await fixture.close();
    }
  },
);

it.skipIf(process.platform !== "linux").each([
  { sessionDisplay: ":99.1", callerDisplay: ":99" },
  { sessionDisplay: ":99", callerDisplay: ":99.1" },
  { sessionDisplay: ":98", callerDisplay: ":99.0" },
])(
  "rejects distinct XFCE display $sessionDisplay for caller $callerDisplay",
  async ({ sessionDisplay, callerDisplay }) => {
    const fixture = themeFixture();
    try {
      await fixture.session(sessionDisplay, "unix:abstract=other-display");
      const result = fixture.run("dark", callerDisplay, "unix:abstract=unrelated-caller");
      expect(result.status).not.toBe(0);
      expect(result.queries).toEqual([]);
      expect(result.components).toBe("");
    } finally {
      await fixture.close();
    }
  },
);

it.skipIf(process.platform !== "linux").each([
  { name: "the inherited session bus", callerBus: "unix:abstract=crabbox-fixture-autostart" },
  { name: "no caller bus during recovery", callerBus: undefined },
  { name: "an unrelated caller bus during recovery", callerBus: "unix:abstract=unrelated-caller" },
])("launches the registered XFCE terminal on its session bus with $name", async ({ callerBus }) => {
  const fixture = themeFixture();
  const bus = "unix:abstract=crabbox-fixture-autostart";
  try {
    await fixture.session(":99", bus);
    const result = fixture.autostart(":99", callerBus, bus);
    expect({ status: result.status, stderr: result.stderr }).toEqual({ status: 0, stderr: "" });
    expect(result.terminals).toEqual([["xfce4-terminal", bus, ":99", "Crabbox Desktop"]]);
    expect(result.queries.length).toBeGreaterThan(0);
    for (const [, actualBus, display] of result.queries) {
      expect(actualBus).toBe(bus);
      expect(display).toBe(":99");
    }
    expect(result.components).toBe("");
  } finally {
    await fixture.close();
  }
});

it.skipIf(process.platform === "win32").each([
  {
    name: "keeps an existing preferred terminal without launching the fallback",
    preferredAvailable: true,
    preferredExisting: true,
    fallbackAvailable: true,
    fallbackExisting: false,
    launch: undefined,
  },
  {
    name: "launches xterm when the preferred terminal binary is unavailable",
    preferredAvailable: false,
    preferredExisting: false,
    fallbackAvailable: true,
    fallbackExisting: false,
    launch: "xterm",
  },
  {
    name: "keeps an existing fallback when the preferred terminal binary is unavailable",
    preferredAvailable: false,
    preferredExisting: false,
    fallbackAvailable: true,
    fallbackExisting: true,
    launch: undefined,
  },
])("registered XFCE autostart $name", async (scenario) => {
  const fixture = themeFixture(scenario);
  const bus = "unix:abstract=crabbox-fixture-terminal-selection";
  try {
    const result = fixture.autostart(":99", bus);
    expect({ status: result.status, stderr: result.stderr }).toEqual({ status: 0, stderr: "" });
    expect(result.terminals).toEqual(
      scenario.launch ? [[scenario.launch, bus, ":99", "Crabbox Desktop"]] : [],
    );
    expect(result.queries).toEqual([]);
    expect(result.components).toBe("");
  } finally {
    await fixture.close();
  }
});

it
  .skipIf(process.platform !== "linux")
  .each(["missing bus", "ambiguous session", "ambiguous screen-zero aliases"] as const)(
  "rejects %s before writing live settings",
  async (scenario) => {
    const fixture = themeFixture();
    try {
      await fixture.session(":99", scenario === "missing bus" ? undefined : "unix:abstract=first");
      if (scenario !== "missing bus") {
        await fixture.session(
          scenario === "ambiguous session" ? ":99" : ":99.0",
          "unix:abstract=second",
        );
      }
      const result = fixture.run(
        "dark",
        ":99",
        "unix:abstract=unrelated-caller",
        "unix:abstract=first",
      );
      expect(result.status).not.toBe(0);
      expect(result.queries).toEqual([]);
      expect(result.components).toBe("");
    } finally {
      await fixture.close();
    }
  },
);

it.skipIf(process.platform === "win32").each(["xfce4-terminal", "xterm"] as const)(
  "detaches the registered %s GUI from the launcher's standard streams",
  async (terminal) => {
    const fixture = themeFixture({
      preferredAvailable: terminal === "xfce4-terminal",
      preferredExisting: false,
      fallbackAvailable: true,
      fallbackExisting: false,
    });
    const launcher = fixture.spawnAutostart(`
import { existsSync, fstatSync, statSync, writeSync, closeSync } from "node:fs";
const input = fstatSync(0), output = fstatSync(1), error = fstatSync(2);
const devNull = statSync("/dev/null");
const logPath = process.env.HOME + "/.cache/crabbox/desktop-terminal.log";
const log = existsSync(logPath) ? statSync(logPath) : undefined;
const sameFile = (a, b) => a.dev === b.dev && a.ino === b.ino;
writeSync(1, "fixture terminal output\\n");
writeSync(2, "fixture terminal diagnostic\\n");
writeSync(3, JSON.stringify({
  terminal: process.argv[2],
  stdinIsDevNull: input.isCharacterDevice() && sameFile(input, devNull),
  stdoutIsDiagnosticLog: Boolean(log && output.isFile() && sameFile(output, log)),
  stderrIsSameLog: error.isFile() && sameFile(error, output),
  logOwnedByUser: Boolean(log && log.uid === process.getuid()),
}) + "\\n");
closeSync(3);
setInterval(() => {}, 1000);
`);
    const exited = once(launcher, "exit");
    const outputClosed = Promise.all([
      once(launcher.stdout!, "end"),
      once(launcher.stderr!, "end"),
    ]);
    void exited.catch(() => {});
    void outputClosed.catch(() => {});
    launcher.stdout!.resume();
    launcher.stderr!.resume();
    const metadata = launcher.stdio[3]!;
    const report = new Promise<string>((resolve, reject) => {
      let text = "";
      metadata.on("data", (chunk: Buffer) => {
        text += chunk.toString();
        const end = text.indexOf("\n");
        if (end >= 0) resolve(text.slice(0, end));
      });
      metadata.once("error", reject);
      metadata.once("end", () => {
        if (!text.includes("\n")) reject(new Error("GUI exited without reporting its descriptors"));
      });
    });
    void report.catch(() => {});
    let timer: ReturnType<typeof setTimeout> | undefined;
    const deadline = new Promise<never>((_, reject) => {
      timer = setTimeout(
        () => reject(new Error("GUI launch or stream closure did not complete")),
        3000,
      );
    });
    try {
      // A wrong descriptor fails immediately; the deadline only bounds process failures.
      const descriptors: unknown = JSON.parse(await Promise.race([report, deadline]));
      expect(descriptors).toEqual({
        terminal,
        stdinIsDevNull: true,
        stdoutIsDiagnosticLog: true,
        stderrIsSameLog: true,
        logOwnedByUser: true,
      });
      expect(await Promise.race([exited, deadline])).toEqual([0, null]);
      await Promise.race([outputClosed, deadline]);
      // The reaped launcher no longer holds this group alive; its GUI still does.
      expect(() => process.kill(-launcher.pid!, 0)).not.toThrow();
      const log = readFileSync(join(fixture.home, ".cache/crabbox/desktop-terminal.log"), "utf8");
      expect(log).toContain("fixture terminal output\n");
      expect(log).toContain("fixture terminal diagnostic\n");
    } finally {
      clearTimeout(timer);
      try {
        if (launcher.pid) stopFixtureProcessGroup(launcher.pid);
      } finally {
        await exited.catch(() => {});
        for (const stream of launcher.stdio) stream?.destroy();
        await fixture.close();
      }
    }
  },
);
