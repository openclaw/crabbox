import { describe, expect, it } from "vitest";

import { cloudInit } from "../src/bootstrap";
import type { LeaseConfig } from "../src/config";

const config: LeaseConfig = {
  provider: "aws",
  target: "linux",
  windowsMode: "normal",
  desktop: false,
  browser: false,
  profile: "project-check",
  class: "standard",
  serverType: "c7a.8xlarge",
  location: "fsn1",
  image: "ubuntu-24.04",
  awsRegion: "eu-west-1",
  awsAMI: "",
  awsSGID: "",
  awsSubnetID: "",
  awsProfile: "",
  awsRootGB: 400,
  capacityMarket: "spot",
  capacityStrategy: "most-available",
  capacityFallback: "on-demand-after-120s",
  capacityRegions: [],
  capacityAvailabilityZones: [],
  sshUser: "crabbox",
  sshPort: "2222",
  sshFallbackPorts: ["22"],
  providerKey: "crabbox-steipete",
  workRoot: "/work/crabbox",
  ttlSeconds: 1200,
  idleTimeoutSeconds: 360,
  keep: false,
  sshPublicKey: "ssh-ed25519 test",
};

describe("cloud-init bootstrap", () => {
  it("uses retrying package installation in runcmd", () => {
    const got = cloudInit(config);
    expect(got).toContain("package_update: false");
    expect(got).toContain("bash -euxo pipefail <<'BOOT'");
    expect(got).toContain('Acquire::Retries "8";');
    expect(got).toContain("retry apt-get update");
    expect(got).toContain(
      "retry apt-get install -y --no-install-recommends openssh-server ca-certificates curl git rsync jq",
    );
    expect(got).toContain("curl --version >/dev/null");
    expect(got).toContain("test -f /var/lib/crabbox/bootstrapped");
    expect(got).toContain("test -w /work/crabbox");
    expect(got).toContain("      Port 2222\n      Port 22");
    expect(got).toContain("touch /var/lib/crabbox/bootstrapped");
    expect(got).not.toContain("\npackages:\n");
    expect(got).not.toContain("go version");
    expect(got).not.toContain("golang-go");
    expect(got).not.toContain("go.dev/dl/go");
    expect(got).not.toContain("/usr/local/go");
    expect(got).not.toContain("node --version");
    expect(got).not.toContain("pnpm --version");
    expect(got).not.toContain("docker --version");
    expect(got).not.toContain("build-essential");
    expect(got).not.toContain("docker.io");
    expect(got).not.toContain("corepack");
  });

  it("adds desktop services only when requested", () => {
    const got = cloudInit({ ...config, desktop: true });
    expect(got).toContain("xvfb xfce4 xfce4-terminal x11vnc xauth dbus-x11");
    expect(got).toContain("/etc/systemd/system/crabbox-xvfb.service");
    expect(got).toContain("/etc/systemd/system/crabbox-desktop.service");
    expect(got).toContain("/usr/local/bin/crabbox-desktop-session");
    expect(got).toContain("/etc/systemd/system/crabbox-desktop-session.service");
    expect(got).toContain("/etc/systemd/system/crabbox-x11vnc.service");
    expect(got).toContain("ExecStart=/usr/bin/startxfce4");
    expect(got).toContain("systemctl is-active --quiet crabbox-desktop.service");
    expect(got).toContain("systemctl is-active --quiet crabbox-desktop-session.service");
    expect(got).toContain("x11-xserver-utils xterm");
    expect(got).toContain("xsetroot -solid '#20242b'");
    expect(got).toContain("xterm -title 'Crabbox Desktop'");
    expect(got).toContain("(umask 077 && openssl rand -base64 18 > /var/lib/crabbox/vnc.password)");
    expect(got).toContain("-rfbauth /var/lib/crabbox/vnc.pass");
    expect(got).toContain("ss -ltn | grep -q '127.0.0.1:5900'");
  });

  it("adds browser setup only when requested", () => {
    const got = cloudInit({ ...config, browser: true });
    expect(got).toContain("https://dl.google.com/linux/linux_signing_key.pub");
    expect(got).toContain("chmod 0644 /etc/apt/trusted.gpg.d/google.asc");
    expect(got).toContain("https://dl.google.com/linux/chrome/deb/");
    expect(got).toContain("google-chrome-stable");
    expect(got).toContain("apt-cache show chromium");
    expect(got).toContain("apt-cache show chromium-browser");
    expect(got).toContain("/var/lib/crabbox/browser.env");
    expect(got).toContain('test -x "$BROWSER"');
    expect(got).toContain('"$BROWSER" --version >/dev/null');
  });
});
