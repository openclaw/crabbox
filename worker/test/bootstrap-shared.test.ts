import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import { awsUserData, windowsBootstrapPowerShell } from "../src/bootstrap";
import {
  googleLinuxSigningKeyFingerprint,
  defaultTailscaleVersion,
  defaultTailscaleAMD64SHA256,
  defaultTailscaleARM64SHA256,
  sharedTailscalePackageInstall,
  sharedTailscalePinnedInstall,
  sharedCodeServerInstall,
  sharedMacOS,
  sharedWindowsCore,
  sharedWindowsRuntime,
  sharedWindowsDesktop,
  sharedWindowsDesktopPrelude,
  sharedWindowsFinalize,
  sharedWindowsHeader,
  sharedWindowsNativePrelude,
  sharedWslTruffleHogInstall,
} from "../src/bootstrap.generated";
import { leaseConfig, type Architecture } from "../src/config";
import type { TargetOS } from "../src/types";

interface Fixture {
  name: string;
  target: TargetOS;
  architecture: Architecture;
  windowsMode?: "normal" | "wsl2";
  desktop?: boolean;
  browser?: boolean;
  code?: boolean;
  tailscale?: boolean;
  tailscaleMode?: "package" | "pinned";
  user: string;
  workRoot: string;
  publicKey: string;
  ports: string[];
}
const fixtures: Fixture[] = JSON.parse(
  readFileSync(new URL("../../testdata/bootstrap/fixtures.json", import.meta.url), "utf8"),
);

describe("shared bootstrap composition fixtures", () => {
  for (const fixture of fixtures) {
    it(`${fixture.name}`, () => {
      // Exercise rendering independently of validation/default policy.
      const config = {
        ...leaseConfig({ provider: "aws", sshPublicKey: "ssh-ed25519 fixture" }),
        target: fixture.target,
        architecture: fixture.architecture,
        windowsMode: fixture.windowsMode ?? "normal",
        desktop: fixture.desktop ?? false,
        browser: fixture.browser ?? false,
        code: fixture.code ?? false,
        tailscale: fixture.tailscale ?? false,
        tailscaleAuthKey: "fixture-not-a-live-key",
        tailscaleInstallMode: fixture.tailscaleMode ?? "package",
        tailscaleVersion: defaultTailscaleVersion,
        tailscaleSHA256: { amd64: defaultTailscaleAMD64SHA256, arm64: defaultTailscaleARM64SHA256 },
        sshUser: fixture.user,
        workRoot: fixture.workRoot,
        sshPublicKey: fixture.publicKey,
        sshPort: fixture.ports[0]!,
        sshFallbackPorts: fixture.ports.slice(1),
      };
      const script =
        config.target === "windows" ? windowsBootstrapPowerShell(config) : awsUserData(config);
      const fragments: string[] = [];
      if (config.target === "windows") {
        fragments.push(
          sharedWindowsHeader(
            config.sshUser,
            config.sshPublicKey,
            config.windowsMode === "wsl2" ? "C:\\crabbox" : config.workRoot,
            fixture.ports,
          ),
          sharedWindowsCore(),
          sharedWindowsRuntime(),
        );
        if (config.windowsMode === "wsl2") {
          fragments.push(sharedWindowsNativePrelude(), sharedWslTruffleHogInstall());
          fragments.push("test -e /proc/sys/fs/binfmt_misc/WSLInterop");
        } else if (config.desktop) {
          fragments.push(sharedWindowsDesktopPrelude(), sharedWindowsDesktop());
        } else {
          fragments.push(sharedWindowsNativePrelude(), sharedWindowsFinalize());
        }
      } else if (config.target === "macos") {
        fragments.push(
          sharedMacOS(config.sshUser, config.sshPublicKey, config.workRoot, fixture.ports),
        );
      } else if (config.code) {
        fragments.push(sharedCodeServerInstall());
      }
      if (config.tailscale) {
        fragments.push(
          config.tailscaleInstallMode === "pinned"
            ? sharedTailscalePinnedInstall(
                defaultTailscaleVersion,
                defaultTailscaleAMD64SHA256,
                defaultTailscaleARM64SHA256,
              )
            : sharedTailscalePackageInstall(),
        );
      }
      if (config.browser) fragments.push(googleLinuxSigningKeyFingerprint);
      for (const fragment of fragments) expect(script).toContain(fragment);
    });
  }
});
