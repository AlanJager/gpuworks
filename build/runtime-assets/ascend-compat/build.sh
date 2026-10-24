#!/usr/bin/env bash
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh OUTPUT.so}
"${CC:-cc}" -std=c11 -O2 -Wall -Wextra -Werror -fPIC -fvisibility=hidden \
  -shared -Wl,-z,relro,-z,now -o "$out" "$here/compat.c" -ldl
sha256sum "$out"
