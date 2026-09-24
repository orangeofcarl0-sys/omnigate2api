@echo off
rem One-click launcher for omnigate2api (just double-click this file).
rem All logic lives in start.sh: ensure the Docker Desktop engine, rebuild the image
rem only when sources changed, docker compose up -d, wait for /healthz, then print
rem account/model counts and the panel URL. This file only locates Git Bash and
rem forwards arguments.
rem Usage: start.cmd [status / restart / stop / down / logs / --rebuild / --help]
rem
rem WHY THIS FILE IS PURE ASCII - do not "helpfully" translate it:
rem cmd.exe re-reads a batch file line by line and interprets bytes in the console
rem codepage. With Chinese (UTF-8) text in here, the GBK decode pairs a trailing byte
rem with the FOLLOWING ASCII character and swallows it: "restart" became "'art' is not
rem recognized as an internal or external command". Switching codepage mid-file also
rem shifts the recorded byte offsets and garbles later lines ("do was unexpected").
rem Chinese explanations belong in start.sh, which Git Bash reads as real UTF-8; the
rem chcp below exists so that start.sh's UTF-8 output renders correctly in this window.
setlocal
cd /d "%~dp0"
chcp 65001 >nul 2>nul

set "BASH="
if exist "%ProgramFiles%\Git\bin\bash.exe" set "BASH=%ProgramFiles%\Git\bin\bash.exe"
if not defined BASH if exist "%ProgramFiles%\Git\usr\bin\bash.exe" set "BASH=%ProgramFiles%\Git\usr\bin\bash.exe"
if not defined BASH if exist "%LOCALAPPDATA%\Programs\Git\bin\bash.exe" set "BASH=%LOCALAPPDATA%\Programs\Git\bin\bash.exe"
if not defined BASH for %%B in (bash.exe) do if not defined BASH set "BASH=%%~$PATH:B"
rem PATH fallback must be Git's: System32\bash.exe is the WSL launcher and cannot run start.sh.
if defined BASH set "GITBASH=%BASH:\Git\=%"
if defined BASH if "%GITBASH%"=="%BASH%" set "BASH="
if defined BASH set "GITBASH="

rem The panel action opens the browser by itself, so a successful run needs no keypress.
set "PANEL="
if /i "%~1"=="panel" set "PANEL=1"

if not defined BASH goto nobash

"%BASH%" "%~dp0start.sh" %*
set "RC=%ERRORLEVEL%"
goto done

:nobash
echo [error] Git Bash not found (bash.exe). Install Git for Windows, or run manually:
echo     docker desktop start
echo     docker compose up -d
set "RC=1"

:done
if "%RC%"=="0" if defined PANEL goto end
echo.
if not "%RC%"=="0" echo [omnigate] startup failed, exit code %RC% - reason above
pause

:end
endlocal & exit /b %RC%
