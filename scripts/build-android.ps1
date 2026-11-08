<#
.SYNOPSIS
  Собирает подписанный APK для раздачи: dist\installers\Rebirth-<версия>.apk.

.DESCRIPTION
  Версия и номер сборки берутся из app\pubspec.yaml (номер сборки становится
  versionCode). APK подписывается постоянным ключом из state\android-signing
  (его делает scripts\make-android-key.ps1). Без него скрипт не работает: APK,
  подписанный чем-то другим, потом не обновится поверх.

  После сборки подпись проверяется: сертификат в APK должен быть сертификатом этого
  ключа, а не отладочным.

  Нужны libawg.so (scripts\build-android-lib.ps1) и app\assets\bootstrap.json.

  APK универсальный: в нём всё для arm64-v8a, armeabi-v7a (32-битные ARM) и x86_64,
  система сама берёт своё. Он весит втрое больше, чем только для arm64, зато ставится
  на любое устройство с Android 7 и новее. Каждой из трёх архитектур нужна своя
  libawg.so: без неё приложение на таком устройстве не запустится, поэтому скрипт
  проверяет, что они есть, и после сборки смотрит, что все они попали в APK.
#>
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$app = Join-Path $root 'app'
$keyDir = Join-Path $root 'state\android-signing'
$props = Join-Path $keyDir 'key.properties'

if (-not (Test-Path $props)) {
    throw "Нет ключа подписи ($props). Сделай его один раз: .\scripts\make-android-key.ps1"
}

# --- version
$match = Get-Content (Join-Path $app 'pubspec.yaml') | ForEach-Object {
    [regex]::Match($_, '^version:\s*(\d+\.\d+\.\d+)\+(\d+)\s*$')
} | Where-Object { $_.Success } | Select-Object -First 1
if (-not $match) { throw "В app\pubspec.yaml нет строки вида 'version: 1.2.3+4'." }
$version = $match.Groups[1].Value
Write-Host "Версия $version, сборка $($match.Groups[2].Value)"

# --- what the build needs
$abis = @('arm64-v8a', 'armeabi-v7a', 'x86_64')
foreach ($abi in $abis) {
    $lib = Join-Path $app "android\app\src\main\jniLibs\$abi\libawg.so"
    if (-not (Test-Path $lib)) { throw "Нет $lib. Собери: .\scripts\build-android-lib.ps1" }
}
$bootstrap = Join-Path $app 'assets\bootstrap.json'
# Made afresh every time, so that what was added since (a place for the
# installers, say) is in this build.
& (Join-Path $PSScriptRoot 'prepare-bootstrap.ps1')

# --- tools for the check afterwards
function Find-Jbr {
    foreach ($c in @("$env:ProgramFiles\Android\Android Studio\jbr", $env:JAVA_HOME)) {
        if ($c -and (Test-Path "$c\bin\keytool.exe")) { return $c }
    }
    throw 'Не найден keytool (Android Studio\jbr или JDK).'
}
$jbr = Find-Jbr
$sdk = if ($env:ANDROID_HOME) { $env:ANDROID_HOME } elseif ($env:ANDROID_SDK_ROOT) { $env:ANDROID_SDK_ROOT } else { "$env:LOCALAPPDATA\Android\Sdk" }
$apksigner = Get-ChildItem "$sdk\build-tools" -Directory | Sort-Object { [version]($_.Name -replace '[^\d.].*$', '') } -Descending |
    ForEach-Object { Join-Path $_.FullName 'apksigner.bat' } | Where-Object { Test-Path $_ } | Select-Object -First 1
if (-not $apksigner) { throw "Не найден apksigner в $sdk\build-tools." }

# --- build
$env:REBIRTH_REQUIRE_RELEASE_KEY = '1'
try {
    Push-Location $app
    try {
        # No --target-platform and no --split-per-abi: one APK for all of them.
        flutter build apk --release
        if ($LASTEXITCODE -ne 0) { throw 'flutter build не сработал' }
    } finally {
        Pop-Location
    }
} finally {
    Remove-Item Env:REBIRTH_REQUIRE_RELEASE_KEY -ErrorAction SilentlyContinue
}
$built = Join-Path $app 'build\app\outputs\flutter-apk\app-release.apk'
if (-not (Test-Path $built)) { throw "Нет $built" }

# Every architecture has to be in the APK with the three files it needs: Flutter's
# engine, the program, and the tunnel. One missing means a device of that kind
# would install the app and then not be able to start it.
Add-Type -AssemblyName System.IO.Compression.FileSystem
$zip = [IO.Compression.ZipFile]::OpenRead($built)
try {
    $names = $zip.Entries | ForEach-Object { $_.FullName }
} finally {
    $zip.Dispose()
}
foreach ($abi in $abis) {
    foreach ($so in 'libflutter.so', 'libapp.so', 'libawg.so') {
        if ($names -notcontains "lib/$abi/$so") { throw "В APK нет lib/$abi/${so}: на таких устройствах приложение не запустится." }
    }
}
Write-Host ("В APK есть все архитектуры: {0}" -f ($abis -join ', '))

# --- the signature is the real key's
$env:JAVA_HOME = $jbr
# Windows PowerShell 5.1 turns any stderr line of a program into an error that
# stops the script, so the tools are run with that switched off and judged by
# their exit code.
$before = $ErrorActionPreference
$ErrorActionPreference = 'Continue'
try {
    $report = & $apksigner verify --verbose --print-certs $built 2>&1 | Out-String
    $code = $LASTEXITCODE
} finally {
    $ErrorActionPreference = $before
}
if ($code -ne 0) { throw "apksigner не принял подпись:`n$report" }
# "Signer #1 certificate SHA-256 digest: ..." in older build-tools, "V2 Signer:
# certificate SHA-256 digest: ..." in newer ones.
$inApk = [regex]::Match($report, 'certificate SHA-256 digest: ([0-9a-fA-F]{64})').Groups[1].Value.ToLower()
if (-not $inApk) { throw "Не нашёл сертификат подписи в отчёте apksigner:`n$report" }

$storePassword = (Get-Content $props | Where-Object { $_ -like 'storePassword=*' } | Select-Object -First 1).Substring('storePassword='.Length)
$env:REBIRTH_KEY_PASSWORD = $storePassword
$ErrorActionPreference = 'Continue'
try {
    $listing = & "$jbr\bin\keytool.exe" -list -v -keystore (Join-Path $keyDir 'rebirth-release.jks') `
        -storepass:env REBIRTH_KEY_PASSWORD 2>&1 | Out-String
} finally {
    $ErrorActionPreference = $before
    Remove-Item Env:REBIRTH_KEY_PASSWORD -ErrorAction SilentlyContinue
}
$ofKey = ([regex]::Match($listing, 'SHA256:\s*([0-9A-Fa-f:]+)').Groups[1].Value -replace ':', '').ToLower()
if (-not $ofKey) { throw 'Не удалось прочитать отпечаток ключа из хранилища.' }
if ($inApk -ne $ofKey) {
    throw "APK подписан не релизным ключом (отпечаток $inApk, а у ключа $ofKey). Раздавать его нельзя."
}

# --- result
$out = Join-Path $root 'dist\installers'
New-Item -ItemType Directory -Force $out | Out-Null
$apk = Join-Path $out "Rebirth-$version.apk"
Copy-Item $built $apk -Force
$hash = (Get-FileHash $apk -Algorithm SHA256).Hash.ToLower()
$size = (Get-Item $apk).Length

Write-Host ''
Write-Host "Готово: $apk"
Write-Host ("  {0:N1} МБ, sha256 {1}" -f ($size / 1MB), $hash)
Write-Host "  подписан релизным ключом, отпечаток сертификата $inApk"
Write-Host ''
Write-Host 'Объявить версию (после загрузки файла в релиз GitHub):'
Write-Host "  dist\awgctl.exe release publish -android `"$apk`" -base-url https://github.com/DNsoul/project-rebirth/releases/download/v$version/ -notes `"...`" -dry-run"
