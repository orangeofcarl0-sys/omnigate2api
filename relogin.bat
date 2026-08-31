@echo off
rem 一键重新登录(浏览器授权): STS 临时凭证有效期 24h,到期后账号被禁用
rem (APIG.0301 decrypt token fail),运行本脚本重新换取凭证。
rem 前提: bin\omnigate2api-login.exe 已存在(Windows 交叉编译产物)。
setlocal
cd /d "%~dp0"
if not exist "bin\omnigate2api-login.exe" (
  echo [错误] 缺少 bin\omnigate2api-login.exe
  echo   构建方法: GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o bin\omnigate2api-login.exe ./cmd/login
  exit /b 1
)
echo 将弹出浏览器完成华为云账号授权; 完成后凭证自动落盘 auths/。
"bin\omnigate2api-login.exe" -auth-dir auths
echo.
echo 完成后重启服务即可: docker compose restart
pause
