crabbox_packages_installed() {
  local package installed
  for package in "$@"; do
    installed="$(dpkg-query -W -f='${Status}' "$package" 2>/dev/null)" || return 1
    case "$installed" in
      'install ok installed'|'hold ok installed') ;;
      *) return 1 ;;
    esac
  done
}

crabbox_install_packages() {
  crabbox_packages_installed "$@" && return 0
  local attempt result=1
  for attempt in 1 2 3; do
    # Prepared images can skip the base refresh and retain obsolete package URLs.
    # Refresh again after an install failure, including a mirror/pocket rollover.
    if timeout 180s apt-get -o APT::Update::Error-Mode=any \
      -o Acquire::Retries=2 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 \
      -o Acquire::Languages=none \
      -o Acquire::IndexTargets::deb::DEP-11::DefaultEnabled=false \
      -o Acquire::IndexTargets::deb::CNF::DefaultEnabled=false update &&
      timeout 300s apt-get -o Acquire::Retries=2 \
        -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 \
        install -y --no-install-recommends "$@"; then
      return 0
    else
      result=$?
    fi
    [ "$attempt" -eq 3 ] || sleep "$((attempt * 5))"
  done
  return "$result"
}

crabbox_existing_browser() {
  local package command binary
  for package in google-chrome-stable chromium chromium-browser; do
    crabbox_packages_installed "$package" || continue
    case "$package" in
      google-chrome-stable) command=google-chrome ;;
      *) command="$package" ;;
    esac
    binary="$(command -v "$command")" || continue
    test -x "$binary" && timeout 5s "$binary" --version >/dev/null 2>&1 || continue
    printf '%s\n' "$binary"
    return 0
  done
  return 1
}
