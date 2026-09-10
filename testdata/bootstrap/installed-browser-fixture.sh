set -euo pipefail
mkdir -p "$FIXTURE_BIN"
case "${BROWSER_PACKAGE:-google-chrome-stable}" in
  google-chrome-stable) fixture_browser=google-chrome ;;
  chromium) fixture_browser=chromium ;;
  *) exit 97 ;;
esac
cat >"$FIXTURE_BIN/$fixture_browser" <<'BROWSER'
#!/bin/sh
test "$1" = --version || exit 97
test "${BROWSER_BROKEN:-0}" = 0 || exit 1
printf 'Google Chrome fixture\n'
BROWSER
chmod +x "$FIXTURE_BIN/$fixture_browser"
export PATH="$FIXTURE_BIN:/usr/bin:/bin"
dpkg-query() {
  test "$3" != "${MISSING_PACKAGE:-}" || return 1
  case "$3" in
    google-chrome-stable|chromium|chromium-browser)
      test "$3" = "${BROWSER_PACKAGE:-google-chrome-stable}" || return 1 ;;
  esac
  if test "$3" = "${BROKEN_PACKAGE:-}"; then
    printf 'deinstall ok config-files'
  elif test "$3" = "${HELD_PACKAGE:-}"; then
    printf 'hold ok installed'
  else
    printf 'install ok installed'
  fi
}
apt-get() {
  printf 'apt-get %s\n' "$*" >>"$FIXTURE_LOG"
  test "${INSTALL_ALLOWED:-0}" = 1 || return 99
  test "${INSTALL_FAIL:-0}" = 0 || return 47
}
curl() { printf 'network forbidden\n' >>"$FIXTURE_LOG"; return 98; }
retry() { "$@"; }
systemctl() { return 0; }
timeout() { shift; "$@"; }
