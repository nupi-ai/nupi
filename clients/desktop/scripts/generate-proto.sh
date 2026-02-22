#!/usr/bin/env sh
set -eu

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname "$0")" && pwd)"
ROOT_DIR="$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)"

LOCAL_BIN="$ROOT_DIR/node_modules/.bin"
MOBILE_BIN="$ROOT_DIR/../mobile/node_modules/.bin"

if [ -x "$LOCAL_BIN/buf" ] && [ -x "$LOCAL_BIN/protoc-gen-es" ]; then
  BIN_DIR="$LOCAL_BIN"
elif [ -x "$MOBILE_BIN/buf" ] && [ -x "$MOBILE_BIN/protoc-gen-es" ]; then
  BIN_DIR="$MOBILE_BIN"
else
  echo "Missing buf/protoc-gen-es. Install desktop deps or ensure clients/mobile/node_modules exists." >&2
  exit 1
fi

cd "$ROOT_DIR"
PATH="$BIN_DIR:$PATH" "$BIN_DIR/buf" generate
