@echo off
cd /d %~dp0

set ADMIN_PASSWORD=admin888
set PORT=8080
set DATA_DIR=%CD%\data
set MV_DIR=%CD%\mv
set MV_NET_DIR=%CD%\mv-net
set SINGER_DIR=%CD%\singer
set WEB_DIR=%CD%\web

echo ============================================
echo   JIAYUE KTV starting...
echo   URL: http://127.0.0.1:8080
echo   MV_DIR: %MV_DIR%
echo   DATA_DIR: %DATA_DIR%
echo   ADMIN_PASSWORD: admin888
echo ============================================
echo.

start "jiayue-ktv-server" "%CD%\ktv-server.exe"

timeout /t 2 >nul
echo Started. Open http://127.0.0.1:8080 in browser
pause