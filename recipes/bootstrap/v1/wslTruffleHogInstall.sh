trufflehog_version={{sh:wslTruffleHogVersion}}
trufflehog_sha256={{sh:wslTruffleHogAMD64SHA256}}
if ! command -v trufflehog >/dev/null 2>&1 || ! trufflehog --no-update --version | grep -Eq {{sh:wslTruffleHogVersionPattern}}; then
  trufflehog_archive="trufflehog_${trufflehog_version}_linux_amd64.tar.gz"
  trufflehog_tmp="$(mktemp -d)"
  curl -fsSL --retry 3 --output "$trufflehog_tmp/$trufflehog_archive" \
    "https://github.com/trufflesecurity/trufflehog/releases/download/v${trufflehog_version}/${trufflehog_archive}"
  (
    cd "$trufflehog_tmp"
    printf '%s  %s\n' "$trufflehog_sha256" "$trufflehog_archive" | sha256sum -c -
  )
  tar --no-same-owner -xzf "$trufflehog_tmp/$trufflehog_archive" -C "$trufflehog_tmp" trufflehog
  trufflehog_candidate="$(mktemp /usr/local/bin/trufflehog.tmp.XXXXXX)"
  install -m 0755 "$trufflehog_tmp/trufflehog" "$trufflehog_candidate"
  if ! "$trufflehog_candidate" --no-update --version | grep -Eq {{sh:wslTruffleHogVersionPattern}}; then
    rm -f "$trufflehog_candidate"
    rm -rf "$trufflehog_tmp"
    exit 1
  fi
  mv -f "$trufflehog_candidate" /usr/local/bin/trufflehog
  rm -rf "$trufflehog_tmp"
fi
trufflehog --no-update --version
