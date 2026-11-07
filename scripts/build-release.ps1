<#
.SYNOPSIS
  Собирает всё, что раздаётся людям, в dist\installers: установщик Windows и APK.

.DESCRIPTION
  Версия и номер сборки берутся из app\pubspec.yaml: перед выпуском их нужно
  поднять там (version: 1.2.0+12), а в app\lib\core\version.dart поставить те же
  (тест это проверяет). Номер сборки должен расти с каждым выпуском: по нему
  приложения решают, что вышла новая версия.

  Порядок выпуска:
    1. поднять версию в pubspec.yaml и version.dart, прогнать тесты;
    2. .\scripts\build-release.ps1
    3. создать релиз на GitHub с тегом v<версия> и загрузить в него оба файла из
       dist\installers (веб-страницей или `gh release create`);
    4. объявить версию подписанным анонсом: команда `awgctl release publish`
       печатается в конце. Сначала с -dry-run, потом без.

  Анонс делается после загрузки файлов, а не до: приложения увидят его при
  ближайшей проверке и сразу пойдут качать.

.PARAMETER SkipWindows
  Не собирать установщик Windows.
.PARAMETER SkipAndroid
  Не собирать APK.
#>
param(
    [switch]$SkipWindows,
    [switch]$SkipAndroid
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot

$match = Get-Content (Join-Path $root 'app\pubspec.yaml') | ForEach-Object {
    [regex]::Match($_, '^version:\s*(\d+\.\d+\.\d+)\+(\d+)\s*$')
} | Where-Object { $_.Success } | Select-Object -First 1
if (-not $match) { throw "В app\pubspec.yaml нет строки вида 'version: 1.2.3+4'." }
$version = $match.Groups[1].Value

# The number built into the program must be the one in pubspec.yaml, or the
# program would offer itself as an update over and over.
$code = Get-Content (Join-Path $root 'app\lib\core\version.dart') -Raw
$codeVersion = [regex]::Match($code, "appVersion\s*=\s*'([^']+)'").Groups[1].Value
$codeBuild = [regex]::Match($code, 'appBuild\s*=\s*(\d+)').Groups[1].Value
if ($codeVersion -ne $version -or $codeBuild -ne $match.Groups[2].Value) {
    throw "В app\lib\core\version.dart версия $codeVersion+$codeBuild, а в pubspec.yaml $version+$($match.Groups[2].Value). Поставь одинаковые."
}

if (-not $SkipWindows) {
    & (Join-Path $PSScriptRoot 'build-installer.ps1')
    if ($LASTEXITCODE -ne 0) { throw 'Установщик Windows не собрался.' }
}
if (-not $SkipAndroid) {
    & (Join-Path $PSScriptRoot 'build-android.ps1')
    if ($LASTEXITCODE -ne 0) { throw 'APK не собрался.' }
}

$out = Join-Path $root 'dist\installers'
$exe = Join-Path $out "Rebirth-Setup-$version.exe"
$apk = Join-Path $out "Rebirth-$version.apk"

Write-Host ''
Write-Host '=== Выпуск готов ==='
$parts = @()
if (Test-Path $exe) { Write-Host "  $exe"; $parts += "-windows `"$exe`"" }
if (Test-Path $apk) { Write-Host "  $apk"; $parts += "-android `"$apk`"" }
Write-Host ''
Write-Host "1. Релиз на GitHub с тегом v$version, в него оба файла."
Write-Host '2. Объявить версию (сначала с -dry-run, потом без):'
Write-Host ("   dist\awgctl.exe release publish {0} -base-url https://github.com/DNsoul/project-rebirth/releases/download/v{1}/ -notes `"Что нового`" -dry-run" -f ($parts -join ' '), $version)
