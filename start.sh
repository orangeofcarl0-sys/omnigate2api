#!/usr/bin/env bash
# start.sh — omnigate2api 一键启动 / 状态 / 停止
#
# 做的事（按顺序，每步都可单独跑）：
#   1. 引擎：Docker Desktop 没起就先起，并等引擎就绪（默认上限 240s）
#   2. 镜像：缺失、或 cmd/internal/go.mod/go.sum/Dockerfile 比镜像新 → 重建（--rebuild 强制）
#   3. 起容器：docker compose up -d
#   4. 等就绪：轮询 /healthz（默认上限 120s），失败自动打印容器日志与状态
#
# 用法:
#   ./start.sh                 # 一键启动（已在跑则只是确认状态，不重建不重启）
#   ./start.sh panel           # 快捷入口：起好了就用默认浏览器打开面板（已在跑则秒开）
#   ./start.sh status          # 看状态：引擎 / 容器 / healthz / 账号 / 模型数
#   ./start.sh restart         # 重启容器并等就绪（改了配置/热加载新账号后用）
#   ./start.sh stop            # 停止容器（保留容器与 data/ 状态；下次启动快）
#   ./start.sh down            # 删除容器（数据仍在 ./data ./auths）
#   ./start.sh logs            # 跟随容器日志（Ctrl-C 退出）
#   ./start.sh --rebuild       # 强制重建镜像
#   ./start.sh --open          # 就绪后用默认浏览器打开面板
#   ./start.sh --timeout 300   # 引擎就绪等待上限（秒）
#   ./start.sh --ready-timeout 180
#
# Windows 直接双击 start.cmd（会找到 bash 并调用本脚本）。
set -u

cd "$(dirname "${BASH_SOURCE[0]}")" || exit 1

ENGINE_TIMEOUT=240
READY_TIMEOUT=120
REBUILD=0
OPEN=0
ACTION=start

while [ $# -gt 0 ]; do
    case "$1" in
        start|panel|status|restart|stop|down|logs) ACTION="$1" ;;
        --rebuild) REBUILD=1 ;;
        --open) OPEN=1 ;;
        --timeout) shift; ENGINE_TIMEOUT="${1:-240}" ;;
        --ready-timeout) shift; READY_TIMEOUT="${1:-120}" ;;
        -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
        *) echo "未知参数: $1（--help 看用法）" >&2; exit 2 ;;
    esac
    shift
done

log()  { printf '[omnigate] %s\n' "$*"; }
warn() { printf '[omnigate] ! %s\n' "$*" >&2; }
die()  { printf '[omnigate] ✗ %s\n' "$*" >&2; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }

# ---- 面板/健康检查端口：OMNIGATE_LISTEN > config.json listen > 7866 ----
port_of() {
    local p="${OMNIGATE_LISTEN:-}"
    if [ -z "$p" ] && [ -f config.json ]; then
        p=$(sed -n 's/.*"listen"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' config.json | head -1)
    fi
    [ -z "$p" ] && p=7866
    printf '%s' "${p##*:}"
}
PORT=$(port_of)
BASE="http://127.0.0.1:${PORT}"

# 本机默认免密；开放方案（设了 OMNIGATE_API_KEY）时带上，否则 401 会让"就绪"判断失真。
api_key() {
    local k="${OMNIGATE_API_KEY:-}"
    if [ -z "$k" ] && [ -f .env ]; then
        k=$(sed -n 's/^OMNIGATE_API_KEY=//p' .env | tail -1 | tr -d '"'"'"'\r')
    fi
    printf '%s' "$k"
}
KEY=$(api_key)
auth_hdr() { [ -n "$KEY" ] && printf 'Authorization: Bearer %s' "$KEY" || printf 'X-Omnigate-No-Key: 1'; }

http_code() { # $1=path
    curl -s -o /dev/null -w '%{http_code}' -m 3 -H "$(auth_hdr)" "${BASE}$1" 2>/dev/null
}

engine_up() { docker info >/dev/null 2>&1; }

ensure_engine() {
    if engine_up; then log "引擎：已就绪"; return 0; fi
    log "引擎：未运行 → 启动 Docker Desktop（首次/冷启动可能 1–3 分钟）"
    if docker desktop version >/dev/null 2>&1; then
        (docker desktop start >/dev/null 2>&1 &)
    elif [ -x "/c/Program Files/Docker/Docker/Docker Desktop.exe" ]; then
        (cmd //c start "" "C:\\Program Files\\Docker\\Docker\\Docker Desktop.exe" >/dev/null 2>&1 &)
    else
        die "找不到 Docker Desktop（docker desktop 插件与默认安装路径都不可用）"
    fi
    local waited=0
    printf '[omnigate] 等待引擎'
    while [ "$waited" -lt "$ENGINE_TIMEOUT" ]; do
        if engine_up; then printf ' 就绪（%ss）\n' "$waited"; return 0; fi
        sleep 5; waited=$((waited + 5)); printf '.'
    done
    printf '\n'
    die "引擎 ${ENGINE_TIMEOUT}s 内未就绪。可试：手动启动 Docker Desktop；或 docker desktop status 看报错"
}

# ---- 镜像是否过期：比对源码内容指纹，而不是时间戳 ----
# 为什么不用"源码 mtime > 镜像创建时间"：Docker 全量缓存命中时（内容没变、只碰了 mtime，
# 或改了又改回）重建出来的镜像 ID 与 Created 都不变，于是每次都判"要重建"——白等一轮。
# 指纹记在 data/（本机状态，gitignore），重建成功后写入；镜像被 prune 掉时按"缺镜像"重建。
FINGERPRINT_FILE=data/.build-fingerprint

src_fingerprint() {
    { find cmd internal -type f \( -name '*.go' -o -name '*.html' \) -print0 2>/dev/null | sort -z | xargs -0 -r sha1sum
      sha1sum Dockerfile go.mod go.sum 2>/dev/null
    } | sha1sum | cut -d' ' -f1
}

# 用 compose 解析出的镜像名（不依赖容器是否存在）——容器被删过也能正确判断"要不要重建"。
image_id() {
    local name
    name=$(docker compose config --images 2>/dev/null | head -1)
    if [ -n "$name" ]; then docker images -q "$name" 2>/dev/null | head -1; return; fi
    docker compose images -q omnigate2api 2>/dev/null | head -1
}

need_build() {
    FINGERPRINT=$(src_fingerprint)
    [ "$REBUILD" = 1 ] && { log "构建：--rebuild 强制重建"; return 0; }
    [ -z "$(image_id)" ] && { log "构建：镜像不存在 → 首次构建"; return 0; }
    local recorded
    recorded=$(cat "$FINGERPRINT_FILE" 2>/dev/null || true)
    if [ "$recorded" != "$FINGERPRINT" ]; then
        if [ -z "$recorded" ]; then
            log "构建：本脚本尚无构建记录 → 重建一次（之后按源码指纹判断）"
        else
            log "构建：源码指纹变化 → 重建"
        fi
        return 0
    fi
    log "构建：源码指纹一致（跳过，省几分钟；要强制用 --rebuild）"
    return 1
}

record_fingerprint() {
    [ -n "${FINGERPRINT:-}" ] || return 0
    mkdir -p "$(dirname "$FINGERPRINT_FILE")" 2>/dev/null
    printf '%s\n' "$FINGERPRINT" > "$FINGERPRINT_FILE" 2>/dev/null || \
        warn "无法写入 $FINGERPRINT_FILE（下次会多重建一次，不影响启动）"
}

preflight() {
    have docker || die "PATH 里没有 docker（Docker Desktop 未安装或未加入 PATH）"
    docker compose version >/dev/null 2>&1 || die "docker compose 不可用（需要 Docker Desktop 或 compose 插件）"
    if [ ! -f config.json ]; then
        warn "config.json 不存在 → 从 config.example.json 复制一份（请按需修改）"
        cp config.example.json config.json || die "复制 config.json 失败"
    fi
    [ -d auths ] || { mkdir -p auths; warn "auths/ 不存在 → 已创建（空池：请先登录账号）"; }
    [ -d data ]  || mkdir -p data
    if ! have curl; then die "PATH 里没有 curl（就绪探测需要；Git Bash / Windows 自带）"; fi
}

accounts_summary() {
    local n_ca n_wb n
    n_ca=$(ls auths/codearts-*.json 2>/dev/null | wc -l | tr -d ' ')
    n_wb=$(ls auths/workbuddy-*.json 2>/dev/null | wc -l | tr -d ' ')
    n=$((n_ca + n_wb))
    printf '账号 %s 个（华为 %s / 腾讯 %s）' "$n" "$n_ca" "$n_wb"
    [ "$n" = 0 ] && printf ' ← 空池，先跑 login.sh / 面板登录'
    printf '\n'
}

models_line() {
    local body n
    body=$(curl -s -m 5 -H "$(auth_hdr)" "${BASE}/v1/models" 2>/dev/null) || return 0
    n=$(printf '%s' "$body" | grep -o '"id"' | wc -l | tr -d ' ')
    [ "$n" -gt 0 ] && printf '模型目录 %s 个\n' "$n"
}

wait_ready() {
    local waited=0
    printf '[omnigate] 等待就绪 %s' "$BASE"
    while [ "$waited" -lt "$READY_TIMEOUT" ]; do
        if [ "$(http_code /healthz)" = 200 ]; then printf ' 就绪（%ss）\n' "$waited"; return 0; fi
        # 容器已退出就没必要等满超时
        if ! docker compose ps --status running -q omnigate2api 2>/dev/null | grep -q .; then
            printf '\n'; warn "容器未在运行，最近日志："
            docker compose logs --tail=25 omnigate2api 2>&1 | sed 's/^/    /'
            return 1
        fi
        sleep 2; waited=$((waited + 2)); printf '.'
    done
    printf '\n'
    warn "healthz ${READY_TIMEOUT}s 未返回 200。容器状态："
    docker compose ps 2>&1 | sed 's/^/    /'
    # 容器可能一直"跑着但没监听"（端口占用/配置错/启动即崩）——尾部日志多为调度器噪音，
    # 单独把可疑行捞出来，并滤掉例行 account=/action= 行（腾讯任务领取失败也算 error，会淹没真因）。
    local sus
    sus=$(docker compose logs --tail=300 omnigate2api 2>&1 |
          grep -iE 'error|fatal|panic|bind|listen|address already in use|refused|no such file' |
          grep -vE 'account=|action=' | tail -12)
    if [ -n "$sus" ]; then
        warn "可疑日志行（近 300 行内筛 error/panic/端口，已滤例行 account= 行）："
        printf '%s\n' "$sus" | sed 's/^/    /'
    fi
    warn "完整日志：bash start.sh logs"
    return 1
}

# 打开浏览器。两种机制都在本机实测过（用一个临时 http.server 看 AccessLog 确认"浏览器真的
# 导航过去了"，而不是只把 URL 当成窗口标题）：PowerShell 的 Start-Process 最稳，cmd start 兜底
# （Git Bash 下 `cmd //c start "" url` 的空标题实参能正确传过去，实测有访问记录）。
open_browser() {
    local url="$1"
    if have powershell.exe || have powershell; then
        local ps=powershell.exe; have "$ps" || ps=powershell
        if "$ps" -NoProfile -Command "Start-Process '$url'" >/dev/null 2>&1; then
            log "已用默认浏览器打开面板：${url}"; return 0
        fi
    fi
    if have cmd; then
        if cmd //c start "" "$url" >/dev/null 2>&1; then
            log "已用默认浏览器打开面板：${url}"; return 0
        fi
    fi
    if have xdg-open; then xdg-open "$url" >/dev/null 2>&1 & log "已调用 xdg-open：${url}"; return 0; fi
    if have open; then open "$url" >/dev/null 2>&1 && { log "已调用 open：${url}"; return 0; }; fi
    warn "没找到可用的浏览器打开方式，请手动访问 ${url}"
    return 1
}

report_ok() {
    local st
    st=$(docker compose ps --format '{{.Status}}' omnigate2api 2>/dev/null | head -1)
    log "容器：${st:-已启动}"
    accounts_summary
    models_line
    log "面板：${BASE}/    客户端 BaseURL：${BASE}/v1"
    if [ -z "$KEY" ]; then
        log "鉴权：关闭（本机免密；局域网/远程共享请设 OMNIGATE_API_KEY 并改 compose 端口映射）"
    else
        log "鉴权：开启（OMNIGATE_API_KEY 已设置，客户端需带 Bearer）"
    fi
    if [ "$OPEN" = 1 ]; then open_browser "${BASE}/" || true; fi
}

do_status() {
    if engine_up; then log "引擎：已就绪"; else log "引擎：未运行（Docker Desktop 未启动）"; fi
    if [ "$(http_code /healthz)" = 200 ]; then
        log "服务：健康 ${BASE}/healthz"
        docker compose ps 2>/dev/null | sed 's/^/    /'
        accounts_summary
        models_line
        log "面板：${BASE}/"
    else
        log "服务：未响应（$BASE）"
        engine_up && docker compose ps 2>&1 | sed 's/^/    /'
    fi
}

do_start() {
    preflight
    ensure_engine
    local build_args=""
    if need_build; then build_args="--build"; fi
    log "启动：docker compose up -d ${build_args}"
    # shellcheck disable=SC2086
    docker compose up -d $build_args || die "docker compose up 失败（上面有 compose 的报错）"
    record_fingerprint
    wait_ready || exit 1
    report_ok
}

# panel：dashboard 快捷入口。已在跑就秒开（不碰镜像/容器），没跑就先按 startup 路径拉起再开。
do_panel() {
    OPEN=1
    if [ "$(http_code /healthz)" = 200 ]; then
        log "服务已在运行 → 直接打开面板"
        report_ok
        return 0
    fi
    log "服务未就绪 → 先启动再打开面板"
    do_start
}

preflight
case "$ACTION" in
    panel)   do_panel ;;
    status)  do_status ;;
    logs)    engine_up || ensure_engine; exec docker compose logs -f --tail=100 omnigate2api ;;
    stop)    engine_up || die "引擎未运行，无需停止"; docker compose stop && log "已停止（docker compose start 或本脚本可再起）" ;;
    down)    engine_up || die "引擎未运行，无需删除"; docker compose down && log "已删除容器（./data ./auths 未动）" ;;
    restart) preflight; engine_up || ensure_engine
             docker compose restart omnigate2api || die "重启失败"
             wait_ready || exit 1; report_ok ;;
    start)   do_start ;;
esac
