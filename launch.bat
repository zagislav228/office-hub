@echo off
setlocal
cd /d "%~dp0"

where node >nul 2>nul
if errorlevel 1 (
  echo Node.js not found. Install Node.js 18+ from nodejs.org, then run this file again.
  pause
  exit /b 1
)

echo Office Hub starting at http://127.0.0.1:8765
echo Keep this window open - it is the local server. Close it or press Ctrl+C to stop.
echo To stop winget jobs, use the "Otmiani" button in the page.

node server.mjs
if errorlevel 1 pause
