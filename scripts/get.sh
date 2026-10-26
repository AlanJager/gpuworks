#!/usr/bin/env bash
# One-line installer for the GPUWorks control plane, published as get.sh at the
# root of every distribution source by scripts/publish_dist.sh:
#
#   bash <(curl -fsSL https://dl.gpuworks.ai/get.sh)
#   bash <(curl -fsSL https://dl.gpuworks.ai/get.sh) --quiet --addr :8800
#
# It picks the fastest reachable source, downloads the archive for this OS and
# architecture, checks it against SHA256SUMS, unpacks it to a temporary
# directory and runs `gpuworks install` with the arguments given here (see
# docs/reference/cli.md for them). The installer copies itself into the install
# directory, so nothing is left behind in the temporary one.
#
# Environment:
#   GPUWORKS_CHANNEL  release or nightly (default: release, falling back to
#                     nightly while no release has been published)
#   GPUWORKS_SOURCE   use this source instead of probing, e.g.
#                     https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com
set -euo pipefail

sources=(
  https://dl.gpuworks.ai
  https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com
  https://gpuworks-dl.obs.cn-east-3.myhuaweicloud.com
)

say() { printf '==> %s\n' "$*" >&2; }
die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

for tool in curl tar; do
  command -v "$tool" >/dev/null || die "$tool is required"
done
if command -v sha256sum >/dev/null; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "sha256sum or shasum is required"
fi

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS $(uname -s); the server runs on Linux and macOS" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

# latest <source> <channel>: the release id the channel points at, or nothing.
latest() { curl -fsSL --connect-timeout 5 --max-time 10 "$1/$2/latest" 2>/dev/null | tr -d '[:space:]'; }

channels=("${GPUWORKS_CHANNEL:-release}")
if [ -z "${GPUWORKS_CHANNEL:-}" ]; then
  channels+=(nightly)
fi

src="" channel="" id=""
if [ -n "${GPUWORKS_SOURCE:-}" ]; then
  src=${GPUWORKS_SOURCE%/}
  for c in "${channels[@]}"; do
    id=$(latest "$src" "$c") && [ -n "$id" ] && channel=$c && break
  done
  [ -n "$id" ] || die "$src serves no ${channels[*]} build"
else
  # The source answering the channel pointer fastest serves the download.
  for c in "${channels[@]}"; do
    best=""
    for s in "${sources[@]}"; do
      t=$(curl -fsS -o /dev/null --connect-timeout 5 --max-time 10 -w '%{time_total}' "$s/$c/latest" 2>/dev/null) || continue
      if [ -z "$best" ] || awk -v a="$t" -v b="$best" 'BEGIN { exit !(a < b) }'; then
        best=$t src=$s
      fi
    done
    if [ -n "$best" ] && id=$(latest "$src" "$c") && [ -n "$id" ]; then
      channel=$c
      break
    fi
  done
  [ -n "$id" ] || die "no source serves a ${channels[*]} build; check the network or set GPUWORKS_SOURCE"
fi
if [ "$channel" = nightly ] && [ -z "${GPUWORKS_CHANNEL:-}" ]; then
  say "no stable release yet; installing the nightly build"
fi

package=gpuworks-$id-$os-$arch
work=$(mktemp -d "${TMPDIR:-/tmp}/gpuworks-get.XXXXXX")
trap 'rm -rf "$work"' EXIT

say "downloading $package ($channel) from $src"
curl -fL --progress-bar -o "$work/$package.tar.gz" "$src/releases/$id/$package.tar.gz" ||
  die "$src has no $package.tar.gz"
curl -fsSL -o "$work/SHA256SUMS" "$src/releases/$id/SHA256SUMS" || die "cannot download SHA256SUMS"
want=$(awk -v f="$package.tar.gz" '$2 == f {print $1}' "$work/SHA256SUMS")
[ -n "$want" ] || die "SHA256SUMS does not list $package.tar.gz"
[ "$(sha256 "$work/$package.tar.gz")" = "$want" ] || die "$package.tar.gz does not match SHA256SUMS"
say "checksum OK"

tar -xzf "$work/$package.tar.gz" -C "$work"
# Under `curl ... | bash` stdin is the script itself; the installer's
# prompts read the terminal instead.
if [ ! -t 0 ] && { true </dev/tty; } 2>/dev/null; then
  "$work/$package/gpuworks" install "$@" </dev/tty
else
  "$work/$package/gpuworks" install "$@"
fi
