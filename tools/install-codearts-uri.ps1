# 注册/注销 codearts:// 协议处理器（HKCU，无需管理员，可逆）
#
#   powershell -ExecutionPolicy Bypass -File tools/install-codearts-uri.ps1            # 注册
#   powershell -ExecutionPolicy Bypass -File tools/install-codearts-uri.ps1 -Uninstall # 注销
#
# 作用：把 codearts:// 指向 tools/codearts-uri-handler.ps1，使华为门户的自定义协议
# 回传能落到本地并被转发到网关回调（详见 HANDOFF §6.5 与 reverse-engineering 附节）。

param([switch]$Uninstall)

$root = Split-Path -Parent $PSScriptRoot
$handler = Join-Path $root "tools\codearts-uri-handler.ps1"
$regPath = "HKCU:\Software\Classes\codearts"

if ($Uninstall) {
    if (Test-Path $regPath) {
        Remove-Item -Path $regPath -Recurse -Force
        Write-Host "已注销 codearts:// 处理器"
    } else {
        Write-Host "codearts:// 未注册，无需注销"
    }
    exit 0
}

if (-not (Test-Path $handler)) {
    Write-Error "找不到处理器脚本：$handler"
    exit 1
}

New-Item -Path $regPath -Force | Out-Null
New-ItemProperty -Path $regPath -Name "(Default)" -Value "URL:codearts Protocol" -PropertyType String -Force | Out-Null
New-ItemProperty -Path $regPath -Name "URL Protocol" -Value "" -PropertyType String -Force | Out-Null
$cmdPath = Join-Path $regPath "shell\open\command"
New-Item -Path $cmdPath -Force | Out-Null
$command = "powershell.exe -NoProfile -ExecutionPolicy Bypass -File `"$handler`" `"%1`""
New-ItemProperty -Path $cmdPath -Name "(Default)" -Value $command -PropertyType String -Force | Out-Null

Write-Host "已注册 codearts:// -> $handler"
Write-Host "验证：reg query `"HKCU\Software\Classes\codearts\shell\open\command`""
Write-Host "浏览器首次触发时会出现「是否打开该应用」的系统确认框，需点击允许。"
