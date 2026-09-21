import type { EC2SpotClient } from "./aws";
import type { PoolAccessBinding, PoolAccessGrant, ProviderPoolAccess } from "./ready-pool-access";
import type { LeaseRecord } from "./types";

// Guest root and the keeper remain trusted, as with existing reusable pools.
// A root-owned generation tombstone fences delayed/replayed SSM installation.
export class AWSPoolAccess implements ProviderPoolAccess {
  constructor(
    private readonly client: Pick<
      EC2SpotClient,
      "findServer" | "runPoolAccessCommand" | "terminateServerAndWait"
    >,
  ) {}

  private async observe(binding: PoolAccessBinding) {
    const machine = await this.client.findServer(binding.resourceID);
    if (!machine || machine.status === "terminated") return undefined;
    if (
      machine.cloudID !== binding.resourceID ||
      machine.labels["lease"] !== binding.leaseID ||
      machine.labels["crabbox"] !== "true" ||
      machine.labels["created_by"] !== "crabbox"
    )
      throw new Error("AWS pool instance binding mismatch");
    return machine;
  }

  async enroll(lease: LeaseRecord): Promise<PoolAccessBinding> {
    const binding: PoolAccessBinding = {
      leaseID: lease.id,
      provider: "aws",
      resourceID: lease.cloudID ?? "",
      scope: lease.region ?? "",
      user: lease.sshUser ?? "ubuntu",
    };
    if (
      lease.target !== "linux" ||
      !/^i-[a-f0-9]{8,17}$/.test(binding.resourceID) ||
      !/^cbx_[a-zA-Z0-9_-]+$/.test(binding.leaseID) ||
      !/^[a-z_][a-z0-9_-]{0,31}$/.test(binding.user) ||
      !binding.scope ||
      lease.network?.awsPrivate
    )
      throw new Error("AWS portable pools require a public Linux SSH lease with SSM");
    if (!(await this.observe(binding))) throw new Error("AWS pool instance is missing");
    const result = await this.client.runPoolAccessCommand(
      binding.resourceID,
      poolEnrollmentScript(binding),
    );
    if (result !== "ENROLLED") throw new Error("AWS pool guest enrollment was not confirmed");
    return binding;
  }

  async install(grant: PoolAccessGrant): Promise<void> {
    if (!(await this.observe(grant.binding))) throw new Error("AWS pool instance is missing");
    const result = await this.client.runPoolAccessCommand(
      grant.binding.resourceID,
      poolInstallScript(grant),
    );
    if (result !== "INSTALLED") throw new Error("AWS pool access installation was not confirmed");
  }

  async revoke(grant: PoolAccessGrant): Promise<{ destroyed: boolean }> {
    if (!(await this.observe(grant.binding))) return { destroyed: true };
    if (grant.result !== "ready") {
      await this.client.terminateServerAndWait(grant.binding.resourceID);
      if (await this.observe(grant.binding)) throw new Error("AWS pool destruction not confirmed");
      return { destroyed: true };
    }
    const result = await this.client.runPoolAccessCommand(
      grant.binding.resourceID,
      poolRevokeScript(grant),
    );
    if (result !== "FENCED")
      throw new Error("AWS pool reboot fencing awaiting observed boot change");
    return { destroyed: false };
  }
}

function quote(value: string) {
  return `'${value.replaceAll("'", "'\\''")}'`;
}
function root(binding: PoolAccessBinding) {
  return `/var/lib/crabbox-pool/${binding.leaseID}`;
}

export function poolEnrollmentScript(binding: PoolAccessBinding): string {
  return `set -euo pipefail
command -v flock >/dev/null
command -v systemd-run >/dev/null
test -r /proc/sys/kernel/random/boot_id
id ${quote(binding.user)} >/dev/null
install -d -m 700 ${quote(root(binding))}
install -d -m 755 /etc/ssh/crabbox-pool
exec 9>${quote(root(binding) + "/lock")}
flock -x 9
if test -f ${quote(root(binding) + "/binding")}; then
  test "$(cat ${quote(root(binding) + "/binding")})" = ${quote(binding.resourceID)}
else
  printf '%s' ${quote(binding.resourceID)} > ${quote(root(binding) + "/binding")}
fi
# A dedicated root-owned key file prevents the borrower from retaining this grant
# by editing its ordinary authorized_keys. Other guest-root activity is trusted.
printf '%s\\n' ${quote("AuthorizedKeysFile .ssh/authorized_keys /etc/ssh/crabbox-pool/%u")} > /etc/ssh/sshd_config.d/00-crabbox-pool.conf
sshd -t
sshd -T -C user=${binding.user},host=localhost,addr=127.0.0.1 | grep -q '^authorizedkeysfile .* /etc/ssh/crabbox-pool/%u$'
systemctl reload ssh.service
printf ENROLLED`;
}

function preamble(grant: PoolAccessGrant): string {
  return `set -euo pipefail
cd ${quote(root(grant.binding))}
exec 9>lock
flock -x 9
test "$(cat binding)" = ${quote(grant.binding.resourceID)}
generation=$(cat generation 2>/dev/null || printf 0)
test "$generation" -le ${grant.generation}
`;
}

export function poolInstallScript(grant: PoolAccessGrant): string {
  const expiry = Math.floor(Date.parse(grant.expiresAt) / 1000);
  const keyFile = `/etc/ssh/crabbox-pool/${grant.binding.user}`;
  const unit = `crabbox-pool-${grant.binding.leaseID}-${grant.generation}`;
  const expiresSSH =
    new Date(expiry * 1000)
      .toISOString()
      .replaceAll(/[-:TZ]/g, "")
      .split(".")[0] + "Z";
  const expireScript = `#!/bin/bash
set -euo pipefail
cd ${quote(root(grant.binding))}
exec 9>lock
flock -x 9
test "$(cat generation)" = ${quote(String(grant.generation))} || exit 0
# Tombstone first: delayed SSM can never restore an expired generation.
printf closed > state
rm -f ${quote(keyFile)}
cat /proc/sys/kernel/random/boot_id > fence-boot
systemctl --no-block reboot
`;
  return (
    preamble(grant) +
    `test "$(date +%s)" -lt ${expiry}
if test "$generation" -eq ${grant.generation}; then
  test "$(cat state)" = active
  test "$(cat fingerprint)" = ${quote(grant.fingerprint)}
  printf INSTALLED
  exit 0
fi
printf '%s' ${grant.generation} > generation
printf closed > state
printf '%s' ${quote(grant.fingerprint)} > fingerprint
printf '%s' ${quote(expireScript)} > expire-${grant.generation}
chmod 700 expire-${grant.generation}
# Install expiry enforcement before publishing a usable key. --on-calendar also
# fires after a forward clock adjustment; authorized_keys independently denies new auth.
printf '%s\\n' '[Unit]' 'Description=Bounded Crabbox pool access' '[Service]' 'Type=oneshot' ${quote(`ExecStart=/bin/bash ${root(grant.binding)}/expire-${grant.generation}`)} > /etc/systemd/system/${unit}.service
printf '%s\\n' '[Unit]' 'Description=Bounded Crabbox pool expiry' '[Timer]' 'OnCalendar=@${expiry}' 'AccuracySec=1s' 'Persistent=true' '[Install]' 'WantedBy=timers.target' > /etc/systemd/system/${unit}.timer
systemctl daemon-reload
systemctl enable --now ${unit}.timer >/dev/null 2>&1
printf '%s\\n' ${quote(`expiry-time="${expiresSSH}",no-agent-forwarding,no-port-forwarding,no-X11-forwarding ${grant.publicKey}`)} > ${quote(keyFile)}
chmod 644 ${quote(keyFile)}
printf active > state
printf INSTALLED`
  );
}

export function poolRevokeScript(grant: PoolAccessGrant): string {
  return (
    preamble(grant) +
    `test "$generation" -eq ${grant.generation}
if test -f fence-boot && test "$(cat state)" = closed && test "$(cat fence-boot)" != "$(cat /proc/sys/kernel/random/boot_id)"; then
  test ! -s ${quote(`/etc/ssh/crabbox-pool/${grant.binding.user}`)}
  printf FENCED
  exit 0
fi
# Removal alone cannot fence an authenticated session. Retain the pre-reboot ID
# so a lost SSM reply is recoverable without inferring success from an API write.
printf closed > state
rm -f ${quote(`/etc/ssh/crabbox-pool/${grant.binding.user}`)}
if ! test -f fence-boot || test "$(cat fence-boot)" != "$(cat /proc/sys/kernel/random/boot_id)"; then
  cat /proc/sys/kernel/random/boot_id > fence-boot
fi
systemctl --no-block reboot
printf REBOOTING`
  );
}
