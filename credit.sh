#!/usr/bin/env bash
# credit.sh — CodeArts 账号登录态日报（token 有效期/状态）
#
# 用法:
#   ./credit.sh            # 人类可读
#   ./credit.sh -json      # 原始 JSON
#   ./credit.sh -uid <id>  # 指定账号
set -euo pipefail
cd "$(dirname "$0")"

BIN=./bin/omnigate2api-credit
if [ ! -x "$BIN" ]; then
    echo "build credit binary ..."
    mkdir -p ./bin
    go build -o "$BIN" ./cmd/credit
fi

exec "$BIN" "$@"
