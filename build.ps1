param(
    [string]$Version = "0.2.0",
    [string]$Output = "dist\NASArchiveSync.exe"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root

$outDir = Split-Path $Output -Parent
if ($outDir) {
    New-Item -ItemType Directory -Force $outDir | Out-Null
}

go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $Output .
if ($LASTEXITCODE -ne 0) {
    throw "build failed"
}

Write-Host "Built $Output (version $Version)"
