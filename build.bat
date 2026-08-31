@echo off
setlocal

set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0

if not exist dist\sing-box.exe (
    echo sing-box.exe not found. Trying to download...
    powershell -ExecutionPolicy Bypass -File setup.ps1
    if errorlevel 1 (
        echo.
        echo Could not download sing-box.exe automatically.
        echo Please manually download sing-box-1.10.1-windows-amd64.zip from
        echo https://github.com/SagerNet/sing-box/releases
        echo Extract it and place sing-box.exe in the dist\ folder.
        pause
        exit /b 1
    )
)

echo Building proxyNet.exe...
go build -ldflags "-H windowsgui -s -w" -o dist\proxyNet.exe .

if errorlevel 1 (
    echo Build failed.
    pause
    exit /b 1
)

echo Build success: dist\proxyNet.exe
echo Put config.json next to dist\proxyNet.exe before running.
pause
