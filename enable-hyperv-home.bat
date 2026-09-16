@echo off
REM enable-hyperv-home.bat - Enable full Hyper-V on Windows 10/11 Home edition
REM Right-click -> Run as administrator, reboot when finished.

net session >nul 2>&1
if %errorlevel% neq 0 (
    echo Please run this script as Administrator.
    pause
    exit /b 1
)

echo Adding Hyper-V packages...
pushd "%~dp0"
dir /b %SystemRoot%\servicing\Packages\*Hyper-V*.mum > hv.txt
for /f %%i in ('findstr /i . hv.txt 2^>nul') do dism /online /norestart /add-package:"%SystemRoot%\servicing\Packages\%%i"
del hv.txt

echo.
echo Enabling Hyper-V feature...
dism /online /enable-feature /featurename:Microsoft-Hyper-V -All

echo.
echo Done! Please REBOOT your computer to activate Hyper-V.
pause
