#!/usr/bin/env bash
# login.sh — CodeArts Agent（华为云账号）登录：浏览器 OAuth2 + PKCE → auths/codearts-{user_id}.json
#
# 用法:
#   ./login.sh              本机有浏览器：自动打开浏览器，回调 + ticket 轮询双通道
#   ./login.sh -print-only  服务器：打印登录链接，任意机器浏览器打开，ticket 轮询下发
set -euo pipefail
cd "$(dirname "$0")"

AUTH_DIR="./auths"
EXTRA=()
if [[ "${1:-}" == "-print-only" ]]; then
    EXTRA+=("-print-only")
    shift
fi
if [[ "${1:-}" == "-auth-dir" ]]; then
    AUTH_DIR="${2:?usage: -auth-dir <dir>}"
    EXTRA+=("-auth-dir" "$AUTH_DIR")
fi
mkdir -p "$AUTH_DIR"

BIN=./bin/omnigate2api-login
if [ ! -x "$BIN" ]; then
    echo "build login binary ..."
    mkdir -p ./bin
    go build -o "$BIN" ./cmd/login
fi

exec "$BIN" "${EXTRA[@]}"
