#Requires -Version 5.1
<#
.SYNOPSIS
  KARMAX Windows installer.

.DESCRIPTION
  Installs karmax.exe and registers a Scheduled Task that runs KARMAX
  AGGRESSIVELY in the background: hidden (no console window), started at logon,
  and kept alive by a supervisor loop that relaunches it within 2 seconds of
  any exit or crash. The task itself is also set to restart on failure.

  Run from the extracted release directory:
    powershell -ExecutionPolicy Bypass -File install.ps1
  Uninstall (removes the task; leaves the binary and data):
    powershell -ExecutionPolicy Bypass -File install.ps1 -Uninstall
#>
param([switch]$Uninstall)

$ErrorActionPreference = 'Stop'
$TaskName   = 'KARMAX'
$InstallDir = Join-Path $env:LOCALAPPDATA 'KARMAX'
$DataDir    = Join-Path $env:USERPROFILE '.karmax'
$Exe        = Join-Path $InstallDir 'karmax.exe'

if ($Uninstall) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    Get-Process karmax -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    Write-Host "KARMAX scheduled task removed. Binary left at $InstallDir."
    return
}

$SelfDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$BinSrc  = Join-Path $SelfDir 'karmax.exe'
if (-not (Test-Path $BinSrc)) { throw "karmax.exe not found next to this script ($BinSrc)" }

Write-Host "==> installing karmax -> $Exe"
New-Item -ItemType Directory -Force -Path $InstallDir, $DataDir | Out-Null
# Stop a running instance so the binary isn't locked during copy.
Get-Process karmax -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Copy-Item -Force $BinSrc $Exe

# Seed %USERPROFILE%\.karmax config on a fresh machine (no-op if it exists).
if (-not (Test-Path (Join-Path $DataDir 'karmax.yaml'))) {
    & $Exe init | Out-Null
}

Write-Host "==> registering scheduled task '$TaskName'"
# The task runs a hidden PowerShell supervisor loop: it relaunches karmax on
# ANY exit (clean or crash), so nothing short of stopping the task takes it down.
$supervisor = "while (`$true) { try { & '$Exe' start } catch {} ; Start-Sleep -Seconds 2 }"
$action = New-ScheduledTaskAction -Execute 'powershell.exe' `
    -Argument "-NoProfile -NonInteractive -WindowStyle Hidden -Command `"$supervisor`"" `
    -WorkingDirectory $DataDir
$trigger = New-ScheduledTaskTrigger -AtLogOn
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -StartWhenAvailable -MultipleInstances IgnoreNew `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartInterval (New-TimeSpan -Minutes 1) -RestartCount 999
$settings.Hidden = $true

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
    -Settings $settings -Description 'KARMAX orchestration daemon (personal AI)' -Force | Out-Null

Start-ScheduledTask -TaskName $TaskName
Start-Sleep -Seconds 2

Write-Host ""
Write-Host "karmax is installed and running in the background."
Get-ScheduledTask -TaskName $TaskName | Select-Object TaskName, State | Format-Table -AutoSize
Write-Host "  status:    schtasks /query /tn $TaskName"
Write-Host "  stop:      Stop-ScheduledTask -TaskName $TaskName"
Write-Host "  uninstall: powershell -ExecutionPolicy Bypass -File install.ps1 -Uninstall"
