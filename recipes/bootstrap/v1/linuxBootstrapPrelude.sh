export DEBIAN_FRONTEND=noninteractive
# APT must not restart cloud-init's single process between boot stages.
# Bootstrap explicitly starts its required services; only report other restarts.
export NEEDRESTART_MODE=l
retry() {
  n=1
  until "$@"; do
    if [ "$n" -ge 8 ]; then
      return 1
    fi
    sleep $((n * 5))
    n=$((n + 1))
  done
}
