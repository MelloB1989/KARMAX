#Requires -Version 5.1
<#
  KARMAX one-line installer (Windows).

    irm https://github.com/MelloB1989/KARMAX/releases/latest/download/install.ps1 | iex

  Downloads the latest Windows release and runs its installer (which registers
  the background scheduled task). Override the source repo with the
  KARMAX_REPO environment variable (owner/repo).
#>
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Repo  = if ($env:KARMAX_REPO) { $env:KARMAX_REPO } else { 'MelloB1989/KARMAX' }
$asset = 'karmax_windows_amd64.zip'
$url   = "https://github.com/$Repo/releases/latest/download/$asset"
$tmp   = Join-Path $env:TEMP ("karmax-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null

try {
    Write-Host "==> downloading $asset"
    Invoke-WebRequest -Uri $url -OutFile (Join-Path $tmp 'karmax.zip') -UseBasicParsing
    Expand-Archive -Path (Join-Path $tmp 'karmax.zip') -DestinationPath $tmp -Force
    Write-Host "==> running installer"
    & (Join-Path $tmp 'karmax\install.ps1')
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
