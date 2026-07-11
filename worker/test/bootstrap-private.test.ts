import { describe, expect, it } from "vitest";

import { awsRunInstancesUserData, cloudInit } from "../src/bootstrap";
import { leaseConfig, type LeaseConfig } from "../src/config";

describe("private AWS cloud-init", () => {
  it("boots as an SSM-only host with HTTPS package sources", () => {
    const got = cloudInit(privateLeaseConfig());

    expect(got).toContain("ssh_pwauth: false");
    expect(got).toContain("disable_root: true");
    expect(got).not.toContain("ssh_authorized_keys");
    expect(got).not.toContain("ssh-ed25519 must-not-appear");
    expect(got).not.toContain("ssh_host_");
    expect(got).not.toContain("/etc/ssh");
    expect(got).not.toContain("openssh-server");
    expect(got).not.toContain("NOPASSWD");
    expect(got).not.toMatch(/systemctl (?:enable|restart|start).*\bssh\b/);
    expect(got).toContain("systemctl disable --now ssh.service ssh.socket");
    expect(got).toContain("systemctl mask ssh.service ssh.socket");
    expect(got).toContain("amazon-ssm-agent.service");
    expect(got).toContain("snap.amazon-ssm-agent.amazon-ssm-agent.service");
    expect(got).toContain("systemctl enable --now amazon-ssm-agent.service");
    expect(got).toContain("systemctl is-active --quiet amazon-ssm-agent.service");
    expect(got).toContain("sed -i 's|http://|https://|g'");
    expect(got).toContain(
      "retry apt-get install -y --no-install-recommends ca-certificates curl git jq util-linux",
    );
    expect(got).toContain(
      "install -d -m 0755 -o root -g root /work/crabbox /work/crabbox/workspaces",
    );
    expect(got).not.toContain("chown -R crabbox:crabbox /work/crabbox");
    expect(got).toContain("test -f /var/lib/crabbox/bootstrapped");
  });

  it("uses the same SSM-only cloud-init in compressed EC2 user data", async () => {
    const encoded = await awsRunInstancesUserData(privateLeaseConfig());
    const got = await gunzipBase64(encoded);

    expect(got).toContain("#cloud-config");
    expect(got).toContain("amazon-ssm-agent.service");
    expect(got).not.toContain("ssh_authorized_keys");
    expect(got).not.toContain("ssh-ed25519 must-not-appear");
    expect(got).not.toContain("openssh-server");
  });
});

function privateLeaseConfig(): LeaseConfig {
  return leaseConfig({
    provider: "aws",
    target: "linux",
    class: "standard",
    serverType: "t3a.small",
    serverTypeExplicit: true,
    awsRegion: "us-west-2",
    awsPrivate: true,
    awsRequireSSM: true,
    awsInstanceTypes: ["t3a.small"],
    awsSubnetID: "subnet-private123",
    awsSGID: "sg-workspace123",
    awsProfile: "crabbox-private-workspace",
    awsRootGB: 20,
    awsSSMBootstrapCommand: "systemctl start crabbox-workspace",
    awsSSMLogGroup: "/crabbox/private-workspaces",
    capacity: { market: "on-demand", fallback: "none", regions: ["us-west-2"] },
    providerKey: "crabbox-workspace-private",
    sshUser: "crabbox",
    sshPublicKey: "ssh-ed25519 must-not-appear",
  });
}

async function gunzipBase64(value: string): Promise<string> {
  const bytes = Uint8Array.from(atob(value), (char) => char.charCodeAt(0));
  const stream = new Blob([bytes]).stream().pipeThrough(new DecompressionStream("gzip"));
  return await new Response(stream).text();
}
