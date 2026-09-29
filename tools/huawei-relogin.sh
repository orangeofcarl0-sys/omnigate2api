#!/usr/bin/env bash
# 华为账号一键重登 / 兜底恢复（HANDOFF §6.5.2、§6.5.3）
#
#   tools/huawei-relogin.sh                    # 默认 http://127.0.0.1:7866
#   tools/huawei-relogin.sh http://host:7866   # 指定网关
#   tools/huawei-relogin.sh --restart         # 完成后自动 docker compose restart（补跑当日福利领取）
#   tools/huawei-relogin.sh --auto            # 定时任务用：有续期路径就跳过，剩余 > 8h 也跳过
#   tools/huawei-relogin.sh --auto --threshold-min 360
#
# 流程：oauth/start 取授权链接 → 打开浏览器（或打印）→ 轮询 oauth/poll 至 done
#       →（可选）重启容器重置当日领取去重。
#
# **定位：这是兜底，不是主路径。** 主路径是授权码 → refresh_token 静默续期（SPEC §24.5）：
# 网关自己每 ~1.5h 刷一次（授权码通道 STS 只有约 2h、refresh_token 有效 30 天），完全不需要
# 浏览器。本脚本用在 refresh_token 被吊销/过期的场合（控制台可吊销客户端会话，2 分钟生效）。
# 所以 `--auto` 会**跳过带续期路径的账号**——否则 2h STS 永远小于任何合理阈值，会天天白弹浏览器。
#
# 重登实测（2026-09-27）：门户会话（浏览器 Cookie）存活时**全程零点击**，约 15 秒后
# oauth/poll 即 done；门户会话过期后浏览器会停在登录页，脚本超时退出（exit 6）→ 人工在
# 浏览器输一次密码即可再续（华为文档：客户端 30 天内重开自动保持登录）。
#
# 定时任务（Windows，每 6 小时）：
#   schtasks /Create /SC HOURLY /MO 6 /TN HuaweiAutoRelogin /TR ^
#     "bash -lc 'cd /f/Codex_Work_Space/omnigate2api && tools/huawei-relogin.sh --auto'"
# 定时任务（Linux 有桌面会话时，crontab -e）：
#   0 */6 * * * cd /path/to/omnigate2api && tools/huawei-relogin.sh --auto >>/tmp/huawei-relogin.log 2>&1
#
# 退出码：0 成功/无需重登 | 2 参数 | 3 缺 python | 4 start 失败 | 5 poll error | 6 超时（需人工）
set -uo pipefail

BASE="http://127.0.0.1:7866"
RESTART=0
AUTO=0
THRESHOLD_MIN=480   # --auto 时：剩余超过这个值就跳过（默认 8h）
while [ $# -gt 0 ]; do
  case "$1" in
    --restart) RESTART=1 ;;
    --auto|--if-needed) AUTO=1 ;;
    --threshold-min)
      shift
      THRESHOLD_MIN="${1:-480}"
      case "$THRESHOLD_MIN" in ''|*[!0-9]*) echo "--threshold-min 需要整数分钟" >&2; exit 2 ;; esac ;;
    http*) BASE="$1" ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
  shift
done

command -v python >/dev/null 2>&1 || { echo "需要 python 解析 JSON" >&2; exit 3; }

# --auto：只在真的需要时才重登（定时任务用）。两条跳过条件：
#   ① 账号已有续期路径（renewal 非空——授权码通道的 refresh_token）：网关自己会刷，
#      不需要浏览器；否则 2h STS 永远小于任何阈值，会天天白弹标签。
#   ② 剩余时间仍大于阈值（默认 8h）。
if [ "$AUTO" = "1" ]; then
  REM=$(curl -s "$BASE/admin/api/overview" | python -c "
import json,sys,datetime
try:
    d=json.load(sys.stdin)
except Exception:
    print('ERR'); raise SystemExit
accs=[a for a in d.get('accounts',[]) if a.get('family')=='codearts']
if not accs:
    print('NONE'); raise SystemExit
# 有续期路径的账号交给网关自刷，本脚本不管它们
need=[a for a in accs if not (a.get('renewal') or '')]
if not need:
    print('AUTO_OK'); raise SystemExit
mins=[]
for a in need:
    e=a.get('expires_at') or ''
    if not e or e.startswith('0001'): continue
    t=datetime.datetime.fromisoformat(e.replace('Z','+00:00'))
    mins.append(int((t-datetime.datetime.now(datetime.timezone.utc)).total_seconds()//60))
print(min(mins) if mins else 'NONE')
" 2>/dev/null)
  case "$REM" in
    ERR) echo "无法读取 overview（$BASE），继续按需重登" ;;
    NONE) echo "==> 账号池里没有华为（codearts）账号，退出"; exit 0 ;;
    AUTO_OK) echo "==> 华为账号都带续期路径（refresh_token），网关自刷，无需重登"; exit 0 ;;
    *)
      if [ "$REM" -gt "$THRESHOLD_MIN" ]; then
        echo "==> 华为凭证剩余 ${REM} 分钟（阈值 ${THRESHOLD_MIN}），无需重登"
        exit 0
      fi
      echo "==> 华为凭证剩余 ${REM} 分钟（阈值 ${THRESHOLD_MIN}），开始重登" ;;
  esac
fi

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
