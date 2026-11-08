<#
.SYNOPSIS
  Проверяет собственный туннель Windows (awgtunnel.exe) от имени администратора.

.DESCRIPTION
  Ставит пробный туннель awgr-smoke, смотрит, что служба запустилась, адаптер
  появился, канал отвечает и журнал пишется, и убирает всё за собой.

  Пробный туннель ничего не меняет в сети: он ведёт на адрес из тестовой сети
  192.0.2.1 (там никого нет), разрешает через себя один адрес 10.255.255.1 и не
  трогает ни имена, ни маршрут по умолчанию. Рукопожатия не будет, так и должно быть.
  Рабочий VPN Rebirth, если он включён, не затрагивается: у туннеля своё имя.

  Запускать в PowerShell «от имени администратора». Служба работает от имени системы,
  поэтому папку с awgtunnel.exe лучше брать защищённую (установленную Rebirth).

.PARAMETER TunnelDir
  Папка с awgtunnel.exe и wintun.dll. По умолчанию tunnel рядом с установленным
  Rebirth, а если его нет, dist\tunnel этого репозитория.
#>
param(
    [string]$TunnelDir
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Запустите PowerShell от имени администратора: без этого службу не поставить.'
}

if (-not $TunnelDir) {
    $installed = Join-Path $env:ProgramFiles 'Rebirth\tunnel'
    $TunnelDir = if (Test-Path (Join-Path $installed 'awgtunnel.exe')) { $installed } else { Join-Path $root 'dist\tunnel' }
}
$exe = Join-Path $TunnelDir 'awgtunnel.exe'
foreach ($f in 'awgtunnel.exe', 'wintun.dll') {
    if (-not (Test-Path (Join-Path $TunnelDir $f))) { throw "Нет $f в $TunnelDir." }
}
Write-Host "Туннель: $exe"

$name = 'awgr-smoke'
$service = "AmneziaWGTunnel`$$name"
$confDir = Join-Path $env:ProgramData 'awg-rotator-smoke'
$conf = Join-Path $confDir "$name.conf"
$results = New-Object System.Collections.ArrayList

function Add-Result([string]$what, [bool]$ok, [string]$detail = '') {
    [void]$results.Add([pscustomobject]@{ Check = $what; Result = $(if ($ok) { 'PASS' } else { 'FAIL' }); Detail = $detail })
}

function New-Key {
    $b = New-Object byte[] 32
    [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
    [Convert]::ToBase64String($b)
}

function Wait-Until([scriptblock]$test, [int]$seconds) {
    $deadline = (Get-Date).AddSeconds($seconds)
    while ((Get-Date) -lt $deadline) {
        if (& $test) { return $true }
        Start-Sleep -Milliseconds 300
    }
    return $false
}

function Remove-Probe {
    & $exe uninstall $name 2>&1 | Out-Null
    [void](Wait-Until { -not (Get-Service $service -ErrorAction SilentlyContinue) } 20)
    if (Test-Path $confDir) { Remove-Item $confDir -Recurse -Force -ErrorAction SilentlyContinue }
}

try {
    Remove-Probe

    New-Item -ItemType Directory -Force $confDir | Out-Null
    # Closed to everyone but administrators and the system, as the app does it.
    & icacls.exe $confDir /inheritance:r /grant:r '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-18:(OI)(CI)F' | Out-Null
    $text = @"
[Interface]
PrivateKey = $(New-Key)
Address = 10.255.255.2/32

[Peer]
PublicKey = $(New-Key)
AllowedIPs = 10.255.255.1/32
Endpoint = 192.0.2.1:51820
PersistentKeepalive = 25
"@
    [IO.File]::WriteAllBytes($conf, (New-Object Text.UTF8Encoding($false)).GetBytes($text))

    $out = & $exe install $conf 2>&1
    Add-Result 'install: службу поставили и запустили' ($LASTEXITCODE -eq 0) ("$out".Trim())

    $running = Wait-Until { (Get-Service $service -ErrorAction SilentlyContinue).Status -eq 'Running' } 45
    Add-Result 'служба работает' $running ("состояние: " + (Get-Service $service -ErrorAction SilentlyContinue).Status)

    $qc = (& sc.exe qc $service 2>&1 | Out-String)
    Add-Result 'служба запускает именно эту программу' ($qc -match [regex]::Escape($exe)) (($qc -split "`r?`n" | Where-Object { $_ -match 'BINARY_PATH_NAME' }) -join '')
    Add-Result 'запуск вручную, не при старте системы' ($qc -match 'DEMAND_START') (($qc -split "`r?`n" | Where-Object { $_ -match 'START_TYPE' }) -join '')

    $adapter = Wait-Until { Get-NetAdapter -Name $name -ErrorAction SilentlyContinue } 20
    Add-Result 'адаптер Wintun появился' ([bool]$adapter) ''

    $hs = & $exe handshake $name 2>&1
    Add-Result 'канал отвечает (рукопожатия ещё нет: 0)' (($LASTEXITCODE -eq 0) -and ("$hs".Trim() -eq '0')) ("ответ: $("$hs".Trim())")

    $log = (& $exe log $confDir 2>&1 | Out-String)
    Add-Result 'журнал службы пишется' ($log -match 'Startup complete') ((($log -split "`r?`n" | Where-Object { $_.Trim() } | Select-Object -Last 2) -join ' | '))

    Remove-Probe
    Add-Result 'после uninstall службы нет' (-not (Get-Service $service -ErrorAction SilentlyContinue)) ''
    $gone = Wait-Until { -not (Get-NetAdapter -Name $name -ErrorAction SilentlyContinue) } 20
    Add-Result 'после uninstall адаптера нет' $gone ''
} finally {
    Remove-Probe
}

Write-Host ''
$results | Format-Table -AutoSize -Wrap | Out-String | Write-Host
if (@($results | Where-Object { $_.Result -eq 'FAIL' }).Count -gt 0) {
    Write-Host 'ЕСТЬ ОШИБКИ' -ForegroundColor Red
    exit 1
}
Write-Host 'ВСЁ В ПОРЯДКЕ' -ForegroundColor Green
