export DEBIAN_FRONTEND=noninteractive
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
