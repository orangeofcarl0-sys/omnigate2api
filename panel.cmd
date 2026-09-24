@echo off
rem Double-click entry for the omnigate2api dashboard (WebUI panel).
rem Starts the gateway if it is not running yet, waits for /healthz, then opens the panel
rem in the default browser. Forwards to start.cmd so that the Git Bash lookup lives in one
rem place; start.cmd skips its "press any key" for the panel action on success, so this
rem window just closes once the browser has the panel (on failure it stays open with the
rem reason, e.g. Docker Desktop could not be started).
rem
rem Keep this file pure ASCII: see tools/check-cmd-ascii.py and HANDOFF section 14 item 16.
call "%~dp0start.cmd" panel %*
exit /b %ERRORLEVEL%
