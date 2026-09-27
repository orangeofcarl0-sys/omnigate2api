#!/usr/bin/env bash
# 腾讯账号一键重登（设备流；SPEC §24.3 / HANDOFF §6.6）
#
#   tools/tencent-relogin.sh                       # 默认 http://127.0.0.1:7866 + 国际（global）
#   tools/tencent-relogin.sh --realm cn            # 国内（codebuddy.cn）
#   tools/tencent-relogin.sh --realm global        # 国际（workbuddy.ai，含 Google/GitHub 入口）
#   tools/tencent-relogin.sh http://host:7866 --realm global
#   tools/tencent-relogin.sh --no-open             # 只打印链接，不尝试开浏览器
#
# 为什么需要它：上游 11140 `request illegal` 是**账号级授权封禁**，社区两个同目标项目
# 一致实测"到期不自愈、必须重新授权登录"（网关侧按硬禁用处理，见 SPEC §28.4 / HANDOFF §7）。
#
# 注意：国际版登录页的 Google 授权**拒绝内嵌 webview**，必须用系统浏览器打开；
#       且要换/指定 Google 账号，否则会授权回同一个 uid（只更新凭证、不新增账号）。
set -uo pipefail

BASE="http://127.0.0.1:7866"
REALM="global"
OPEN=1
for arg in "$@"; do
  case "$arg" in
    --realm) shift ;;
    cn|global) REALM="$arg" ;;
    --no-open) OPEN=0 ;;
    http*) BASE="$arg" ;;
    *) echo "未知参数: $arg" >&2; exit 2 ;;
  esac
done

command -v python >/dev/null 2>&1 || { echo "需要 python 解析 JSON" >&2; exit 3; }

echo "==> 发起腾讯设备流（realm=$REALM）"
START=$(curl -s -X POST "$BASE/admin/api/oauth/tencent/start" \
  -H "Content-Type: application/json" -d "{\"realm\":\"$REALM\"}") || { echo "start 失败：$START" >&2; exit 4; }
ST=$(printf '%s' "$START" | python -c "import json,sys; print(json.load(sys.stdin).get('state',''))")
URL=$(printf '%s' "$START" | python -c "import json,sys; print(json.load(sys.stdin).get('auth_url',''))")
[ -n "$ST" ] && [ -n "$URL" ] || { echo "start 响应异常：$START" >&2; exit 4; }

echo "==> 授权链接（state 约 10 分钟有效，超时重跑本脚本）："
echo "$URL"
if [ "$OPEN" = "1" ]; then
  if command -v powershell >/dev/null 2>&1; then
    powershell -NoProfile -c "Start-Process '$URL'" >/dev/null 2>&1 && echo "（已尝试在系统浏览器打开——国际版必须用系统浏览器，内嵌 webview 会被 Google 拒绝）"
  elif command -v xdg-open >/dev/null 2>&1; then
    xdg-open "$URL" >/dev/null 2>&1 && echo "（已尝试在系统浏览器打开）"
  else
    echo "（无图形环境：请在其他机器的浏览器打开上面的链接）"
  fi
fi

echo "==> 等待浏览器完成授权（每 5s 轮询；上游 state 约 10 分钟有效，故只等 8.5 分钟）"
for i in $(seq 1 100); do
  sleep 5
  OUT=$(curl -s -X POST "$BASE/admin/api/oauth/tencent/poll" \
    -H "Content-Type: application/json" -d "{\"state\":\"$ST\"}")
  OK=$(printf '%s' "$OUT" | python -c "
import json,sys
try: print(json.load(sys.stdin).get('ok',''))
except Exception: print('')" 2>/dev/null)
  WAIT=$(printf '%s' "$OUT" | python -c "
import json,sys
try: print(json.load(sys.stdin).get('waiting',''))
except Exception: print('')" 2>/dev/null)
  EXPIRED=$(printf '%s' "$OUT" | python -c "
import json,sys
try: print(json.load(sys.stdin).get('expired',''))
except Exception: print('')" 2>/dev/null)
  case "$OK" in
    True)
      echo "==> 登录成功：$OUT"
      exit 0 ;;
  esac
  if [ "$EXPIRED" = "True" ]; then
    echo "state 已失效（超时或网关重启）——请重新运行本脚本" >&2
    exit 5
  fi
  [ "$WAIT" = "True" ] || echo "（未完成：$OUT）"
done
echo "轮询超时：请确认已在浏览器完成授权，或重新运行本脚本" >&2
exit 6
