<#
.SYNOPSIS
  Собирает установщик Rebirth для Windows: dist\installers\Rebirth-Setup-<версия>.exe.

.DESCRIPTION
  Версия и номер сборки берутся из app\pubspec.yaml. Сначала собирается само
  приложение (scripts\build-windows.ps1, вариант с запросом прав администратора),
  затем Inno Setup (6.3 и новее, подходит и 7) упаковывает его по installer\rebirth.iss.

  Inno Setup ищется в ISCC.exe из -Iscc, переменной ISCC, PATH и обычных папках
  установки. Скачать: https://jrsoftware.org/isdl.php

  В конце печатаются размер и SHA-256 и готовая команда для объявления версии.
  Установщик не подписан сертификатом, поэтому SmartScreen при первом запуске
  предупредит: это ожидаемо.

.PARAMETER NoBuild
  Не пересобирать приложение, взять то, что лежит в dist\rebirth-windows.
#>
param(
    [switch]$NoBuild,
    [string]$Iscc
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot

# --- the version the app is built as
$pubspec = Join-Path $root 'app\pubspec.yaml'
$match = Get-Content $pubspec | ForEach-Object {
    [regex]::Match($_, '^version:\s*(\d+\.\d+\.\d+)\+(\d+)\s*$')
} | Where-Object { $_.Success } | Select-Object -First 1
if (-not $match) { throw "В $pubspec нет строки вида 'version: 1.2.3+4'." }
$version = $match.Groups[1].Value
$build = $match.Groups[2].Value
Write-Host "Версия $version, сборка $build"

# --- Inno Setup
function Find-Iscc {
    if ($Iscc) { return $Iscc }
    if ($env:ISCC) { return $env:ISCC }
    $onPath = Get-Command ISCC.exe -ErrorAction SilentlyContinue
    if ($onPath) { return $onPath.Source }
    $candidates = @(
        "${env:ProgramFiles(x86)}\Inno Setup 7\ISCC.exe",
        "$env:ProgramFiles\Inno Setup 7\ISCC.exe",
        "$env:LOCALAPPDATA\Programs\Inno Setup 7\ISCC.exe",
        "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe",
        "$env:ProgramFiles\Inno Setup 6\ISCC.exe",
        "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe"
    )
    foreach ($c in $candidates) { if (Test-Path $c) { return $c } }
    return $null
}
$iscc = Find-Iscc
if (-not $iscc) {
    throw 'Не найден Inno Setup (ISCC.exe). Скачай и установи его: https://jrsoftware.org/isdl.php'
}

# --- the app
$appDir = Join-Path $root 'dist\rebirth-windows'
if (-not $NoBuild) {
    & (Join-Path $PSScriptRoot 'build-windows.ps1')
    if ($LASTEXITCODE -ne 0) { throw 'Не собралось приложение.' }
}
$exe = Join-Path $appDir 'Rebirth.exe'
if (-not (Test-Path $exe)) { throw "Нет ${exe}: сначала собери приложение (scripts\build-windows.ps1)." }

# An installer of a build that does not ask for administrator rights would put in
# a program that cannot connect. Looked for in the program itself, as the build
# script does.
$bytes = [IO.File]::ReadAllBytes($exe)
$text = [Text.Encoding]::ASCII.GetString($bytes)
if ($text -notmatch 'requireAdministrator') {
    throw "$exe собран без запроса прав администратора (-NoElevation). Для установщика нужна обычная сборка."
}

# --- the installer
& $iscc "/DVersion=$version" "/DBuild=$build" (Join-Path $root 'installer\rebirth.iss')
if ($LASTEXITCODE -ne 0) { throw 'Inno Setup не справился.' }

$setup = Join-Path $root "dist\installers\Rebirth-Setup-$version.exe"
if (-not (Test-Path $setup)) { throw "Установщик не появился: $setup" }
$hash = (Get-FileHash $setup -Algorithm SHA256).Hash.ToLower()
$size = (Get-Item $setup).Length

Write-Host ''
Write-Host "Готово: $setup"
Write-Host ("  {0:N1} МБ, sha256 {1}" -f ($size / 1MB), $hash)
Write-Host ''
Write-Host 'Объявить версию (после загрузки файла в релиз GitHub):'
Write-Host "  dist\awgctl.exe release publish -windows `"$setup`" -base-url https://github.com/DNsoul/project-rebirth/releases/download/v$version/ -notes `"...`" -dry-run"
