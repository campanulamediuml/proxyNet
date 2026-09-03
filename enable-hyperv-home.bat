@echo off
chcp 65001 >nul
:: enable-hyperv-home.bat - 在 Win10/Win11 家庭版上启用完整 Hyper-V
:: 右键以管理员身份运行，完成后重启电脑

net session >nul 2>&1
if %errorlevel% neq 0 (
    echo 请右键【以管理员身份运行】此脚本
    pause
    exit /b 1
)

echo 正在添加 Hyper-V 组件包...
pushd "%~dp0"
dir /b %SystemRoot%\servicing\Packages\*Hyper-V*.mum > hv.txt
for /f %%i in ('findstr /i . hv.txt 2^>nul') do dism /online /norestart /add-package:"%SystemRoot%\servicing\Packages\%%i"
del hv.txt

echo.
echo 正在启用 Hyper-V 功能...
dism /online /enable-feature /featurename:Microsoft-Hyper-V -All

echo.
echo 完成！请重启电脑使 Hyper-V 生效。
echo 重启后可以使用 Hyper-V 管理器和 setup-vm.ps1
pause
