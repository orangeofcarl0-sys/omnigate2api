# 在桌面（可选开始菜单）创建 omnigate2api 面板的快捷入口。
#
# 双击该快捷方式 = 网关没起就先起（含拉起 Docker Desktop）→ 等 /healthz → 用默认浏览器打开面板，
# 等价于命令行里的 `bash start.sh panel` 或双击 panel.cmd；已在跑时约 4 秒直接开面板。
#
# 用法（PowerShell）:
#   powershell -NoProfile -ExecutionPolicy Bypass -File tools/create-shortcuts.ps1
#   ... -StartMenu      # 顺带建到开始菜单
#   ... -Remove         # 删除本脚本建的快捷方式
#
# 注意：本文件是 UTF-8 **带 BOM**（与 tools/ 下既有 .ps1 一致）。Windows PowerShell 5.1
# 对无 BOM 的 .ps1 按 ANSI(GBK) 读，中文会变乱码——别把 BOM 去掉。
param(
    [switch]$StartMenu,
    [switch]$Remove
)

$ErrorActionPreference = 'Stop'

$repo = Split-Path -Parent $PSScriptRoot
$target = Join-Path $repo 'panel.cmd'
if (-not (Test-Path $target)) { throw "找不到入口文件: $target" }

$dirs = @([Environment]::GetFolderPath('Desktop'))
if ($StartMenu) {
    $dirs += (Join-Path ([Environment]::GetFolderPath('StartMenu')) 'Programs')
}

$shell = New-Object -ComObject WScript.Shell
foreach ($dir in $dirs) {
    if (-not (Test-Path $dir)) { Write-Warning "目录不存在，跳过: $dir"; continue }
    $lnk = Join-Path $dir 'OmniGate 面板.lnk'
    if ($Remove) {
        if (Test-Path $lnk) { Remove-Item $lnk -Force; Write-Host "已删除 $lnk" }
        else { Write-Host "不存在，无需删除: $lnk" }
        continue
    }
    $sc = $shell.CreateShortcut($lnk)
    $sc.TargetPath = $target
    $sc.WorkingDirectory = $repo
    $sc.Description = 'omnigate2api 面板：未运行则先启动网关，再用默认浏览器打开'
    $sc.Save()
    Write-Host "已创建 $lnk"
}
if (-not $Remove) {
    Write-Host ""
    Write-Host "双击即开面板。删除：同一个脚本加 -Remove"
}
