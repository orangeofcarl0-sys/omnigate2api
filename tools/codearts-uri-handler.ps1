# codearts:// URL 协议处理器（Windows，登录 code 通道排查/启用辅助）
#
# 背景（docs/reverse-engineering.md 附节）：华为门户的 OAuth 授权确认疑似依赖
# uri_scheme=codearts 的自定义协议回传（官方客户端注册了该协议，纯浏览器没有处理器
# 时停在「登录失败」页）。注册本处理器后，门户的回传会落到这里并被转发到本网关注册的
# 回调端点，从而走通 authorization_code 通道（该通道据记录会下发 refresh_token，
# 使我们能自动续期、免去每日重登）。
#
# 安全纪律（与网关一致）：
#   - 只处理 codearts:// 的查询串并转发到 127.0.0.1 本地回调，不向任何外部服务发送；
#   - 日志只记长度与参数名，不记 code/secret 值。
#
# 用法：由 tools/install-codearts-uri.ps1 注册到 HKCU\Software\Classes\codearts。

param([Parameter(Position = 0)][string]$Uri = "")

$logDir = Join-Path $env:LOCALAPPDATA "codearts-uri-handler"
$logFile = Join-Path $logDir "handler.log"
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

function Write-Log([string]$Message) {
    Add-Content -Path $logFile -Value ("{0} {1}" -f (Get-Date -Format "yyyy-MM-dd HH:mm:ss"), $Message)
}

if ([string]::IsNullOrWhiteSpace($Uri)) {
    Write-Log "invoked with empty uri"
    exit 1
}

Write-Log ("invoked uri_len={0} scheme_prefix={1}" -f $Uri.Length, ($Uri -replace '\?.*$', ''))

$query = ""
$qIndex = $Uri.IndexOf("?")
if ($qIndex -ge 0) { $query = $Uri.Substring($qIndex + 1) }

# 参数名清单（不记值）：确认门户回传了哪些字段（code / ticket_id / secret / redirect）
$names = @()
if ($query) {
    foreach ($pair in $query.Split("&")) {
        if ($pair -match '^([^=]+)=') { $names += $Matches[1] }
    }
}
Write-Log ("query_params=[{0}]" -f ($names -join ","))

if (-not $query) {
    Write-Log "no query string; nothing to forward"
    exit 2
}

$target = "http://127.0.0.1:7866/oauth/callback?" + $query
try {
    $resp = Invoke-WebRequest -Uri $target -MaximumRedirection 0 -TimeoutSec 15 -UseBasicParsing -ErrorAction Stop
    Write-Log ("forwarded status={0} bytes={1}" -f $resp.StatusCode, $resp.RawContentLength)
} catch {
    # 3xx（网关 307 回门户）在此按重定向异常抛出：记录状态码即可，属预期路径
    $status = $null
    if ($_.Exception.Response) { $status = [int]$_.Exception.Response.StatusCode }
    Write-Log ("forward result status={0} note={1}" -f $status, $_.Exception.Message.Substring(0, [Math]::Min(120, $_.Exception.Message.Length)))
}
