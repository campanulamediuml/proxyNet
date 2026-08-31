# Download sing-box Windows binary for proxyNet
param(
    [string]$Version = "1.13.19",
    [string]$Arch = "amd64"
)

$ErrorActionPreference = "Stop"

# Some mirrors have expired certificates; bypass validation for this download.
[System.Net.ServicePointManager]::ServerCertificateValidationCallback = { $true }
[System.Net.ServicePointManager]::SecurityProtocol = [System.Net.SecurityProtocolType]::Tls12

$DistDir = Join-Path $PSScriptRoot "dist"
$TempDir = Join-Path $DistDir "_temp"
$ZipFile = Join-Path $DistDir "sing-box-${Version}-windows-${Arch}.zip"
$TargetExe = Join-Path $DistDir "sing-box.exe"

if (-not (Test-Path $DistDir)) {
    New-Item -ItemType Directory -Path $DistDir | Out-Null
}

$FileName = "sing-box-${Version}-windows-${Arch}.zip"
$Urls = @(
    "https://ghproxy.net/https://github.com/SagerNet/sing-box/releases/download/v${Version}/${FileName}"
    "https://github.com/SagerNet/sing-box/releases/download/v${Version}/${FileName}"
    "https://ghproxy.com/https://github.com/SagerNet/sing-box/releases/download/v${Version}/${FileName}"
    "https://mirror.ghproxy.com/https://github.com/SagerNet/sing-box/releases/download/v${Version}/${FileName}"
    "https://github.moeyy.xyz/https://github.com/SagerNet/sing-box/releases/download/v${Version}/${FileName}"
)

$Downloaded = $false
foreach ($Url in $Urls) {
    Write-Host "Trying $Url ..."
    try {
        Invoke-WebRequest -Uri $Url -OutFile $ZipFile -TimeoutSec 300 -UseBasicParsing
        $Downloaded = $true
        Write-Host "Downloaded from $Url"
        break
    } catch {
        Write-Host "Failed: $_"
    }
}

if (-not $Downloaded) {
    Write-Host "`nERROR: Could not download sing-box automatically."
    Write-Host "Please manually download:" 
    Write-Host "  ${FileName}"
    Write-Host "from https://github.com/SagerNet/sing-box/releases"
    Write-Host "Extract it and place sing-box.exe at: $TargetExe"
    exit 1
}

if (Test-Path $TempDir) {
    Remove-Item -Recurse -Force $TempDir
}
New-Item -ItemType Directory -Path $TempDir | Out-Null

Expand-Archive -Path $ZipFile -DestinationPath $TempDir -Force

$Exe = Get-ChildItem -Path $TempDir -Filter "sing-box.exe" -Recurse | Select-Object -First 1
if (-not $Exe) {
    Write-Host "ERROR: sing-box.exe not found in extracted archive."
    exit 1
}

Move-Item -Path $Exe.FullName -Destination $TargetExe -Force
Remove-Item -Recurse -Force $TempDir
Remove-Item -Path $ZipFile -Force

Write-Host "sing-box.exe is ready at $TargetExe"
