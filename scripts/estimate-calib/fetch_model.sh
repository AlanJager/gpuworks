#!/usr/bin/env bash
# Fetch the *config-only* slice of a ModelScope repo into
# /opt/gpuworks/models/modelscope/<dir> - everything vLLM needs to build the
# model and profile memory under --load-format dummy, without the weights.
#
#   usage: fetch_model.sh <owner/repo> <dir> [Revision]
set -u

REPO="$1"; DIR="$2"; REV="${3:-master}"
DEST="/opt/gpuworks/models/modelscope/$DIR"
BASE="https://modelscope.cn/api/v1/models/$REPO/repo?Revision=$REV&FilePath="

mkdir -p "$DEST"

FILES=(
  config.json
  generation_config.json
  tokenizer.json
  tokenizer_config.json
  tokenizer.model
  vocab.json
  merges.txt
  special_tokens_map.json
  chat_template.jinja
  preprocessor_config.json
  processor_config.json
)

for f in "${FILES[@]}"; do
  code=$(curl -sL -o "$DEST/$f" -w '%{http_code}' "${BASE}${f}")
  if [ "$code" != "200" ]; then rm -f "$DEST/$f"; fi
done

ls -la "$DEST"
test -f "$DEST/config.json" || { echo "FATAL: no config.json for $REPO" >&2; exit 1; }
