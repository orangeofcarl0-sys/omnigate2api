#!/usr/bin/env bash
# apply.sh — 批量刷新即将过期的 token（对应其他项目的自动签到）
#
# 用法:
#   ./apply.sh            # 仅刷新临期/过期账号
#   ./apply.sh -force     # 全部强制刷新
set -e
cd "$(dirname "$0")"

BIN=./bin/omnigate2api-apply
if [ ! -x "$BIN" ]; then
    echo "build apply binary ..."
    mkdir -p ./bin
    go build -o "$BIN" ./cmd/apply
fi

exec "$BIN" "$@"
