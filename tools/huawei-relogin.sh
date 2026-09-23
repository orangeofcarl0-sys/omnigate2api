#!/usr/bin/env bash
# 华为账号一键重登（HANDOFF §6.5：STS 约 24h 且无 refresh_token，需周期性重登）
#
#   tools/huawei-relogin.sh                    # 默认 http://127.0.0.1:7866
#   tools/huawei-relogin.sh http://host:7866   # 指定网关
#   tools/huawei-relogin.sh --restart         # 完成后自动 docker compose restart（补跑当日福利领取）
#
# 流程：oauth/start 取授权链接 → 打开浏览器（或打印）→ 轮询 oauth/poll 至 done
#       →（可选）重启容器重置当日领取去重。
# 说明：门户页面可能在最后显示「登录失败」，属已知现象——以 oauth/poll 的 done 为准
#       （ticket 通道成功即凭证到手）。详见 docs/reverse-engineering.md 附节。
set -uo pipefail

BASE="http://127.0.0.1:7866"
RESTART=0
for arg in "$@"; do
  case "$arg" in
    --restart) RESTART=1 ;;
    http*) BASE="$arg" ;;
    *) echo "未知参数: $arg" >&2; exit 2 ;;
  esac
done

command -v python >/dev/null 2>&1 || { echo "需要 python 解析 JSON" >&2; exit 3; }

echo "==> 发起授权会话"
START=$(curl -s -X POST "$BASE/admin/api/oauth/start") || { echo "start 失败：$START" >&2; exit 4; }
SID=$(printf '%s' "$START" | python -c "import json,sys; print(json.load(sys.stdin).get('session_id',''))")
URL=$(printf '%s' "$START" | python -c "import json,sys; print(json.load(sys.stdin).get('auth_url',''))")
[ -n "$SID" ] && [ -n "$URL" ] || { echo "start 响应异常：$START" >&2; exit 4; }

echo "==> 授权链接（15 分钟内有效）："
echo "$URL"
if command -v powershell >/dev/null 2>&1; then
  powershell -NoProfile -c "Start-Process '$URL'" >/dev/null 2>&1 && echo "（已尝试在默认浏览器打开）"
elif command -v xdg-open >/dev/null 2>&1; then
  xdg-open "$URL" >/dev/null 2>&1 && echo "（已尝试在默认浏览器打开）"
else
  echo "（无图形环境：请在其他机器的浏览器打开上面的链接）"
fi

echo "==> 等待浏览器完成登录（每 10s 轮询，最多 15 分钟）"
for i in $(seq 1 90); do
  sleep 10
  OUT=$(curl -s -X POST "$BASE/admin/api/oauth/poll" -H "Content-Type: application/json" -d "{\"session_id\":\"$SID\"}")
  ST=$(printf '%s' "$OUT" | python -c "
import json,sys
try: print(json.load(sys.stdin).get('status',''))
except Exception: print('')" 2>/dev/null)
  case "$ST" in
    done)
      echo "==> 登录成功：$OUT"
      if [ "$RESTART" = "1" ]; then
        echo "==> 重启容器（重置当日福利领取去重）"
        docker compose restart 2>&1 | tail -1
        sleep 10
        docker logs "$(docker ps --filter name=omnigate2api --format '{{.Names}}' | head -1)" --since 1m 2>&1 | grep -i "benefit claim" | tail -2
      fi
      exit 0 ;;
    error)
      echo "登录失败：$OUT" >&2
      exit 5 ;;
  esac
done
echo "轮询超时：请确认已在浏览器完成登录，或重新运行本脚本" >&2
exit 6
