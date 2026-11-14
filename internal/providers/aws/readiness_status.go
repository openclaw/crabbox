package aws

// Observe the existing boot owners, without creating markers, restarting units,
// or replacing their readiness predicates. Only fixed checkpoint waits use it.
const awsLinuxReadinessStatus = `
if test -f /run/crabbox/workspace-ready; then echo 'pending readiness-check'; exit 0; fi
for unit in cloud-config crabbox-bootstrap cloud-final crabbox-workspace-ready; do
  if systemctl is-failed --quiet "$unit.service"; then
    case "$unit" in
      crabbox-bootstrap) echo 'failed bootstrap';;
      crabbox-workspace-ready) echo 'failed workspace-ready';;
      *) echo "failed $unit";;
    esac
    exit 0
  fi
done
if ! test -f /var/lib/crabbox/bootstrapped; then
  if test "$(systemctl show --property=ActiveState --value cloud-config.service)" = activating; then echo 'pending cloud-config'; exit 0; fi
  pid=$(systemctl show --property=MainPID --value crabbox-bootstrap.service)
  case "$pid" in ''|0|*[!0-9]*) ;; *)
    if pgrep -P "$pid" -x chown >/dev/null; then echo 'pending ownership-repair'; exit 0; fi;;
  esac
fi
if test ! -f /var/lib/crabbox/bootstrapped; then echo 'pending bootstrap'
elif test "$(systemctl show --property=ActiveState --value cloud-final.service)" = activating; then echo 'pending cloud-final'
elif systemctl cat crabbox-workspace-ready.service >/dev/null 2>&1; then echo 'pending workspace-ready'
else echo 'pending unknown'
fi
`
