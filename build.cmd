@echo off
rem Double-click to build the one-click Tailscale installers into .\dist
rem (runs build.ps1 without changing your PowerShell execution policy).
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0build.ps1" %*
echo.
pause
