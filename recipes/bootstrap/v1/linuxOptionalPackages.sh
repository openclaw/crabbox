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
  if ! crabbox_packages_installed "$@"; then
    retry apt-get install -y --no-install-recommends "$@"
  fi
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
