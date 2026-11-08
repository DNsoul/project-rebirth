<#
.SYNOPSIS
  Собирает libawg.so (туннель AmneziaWG) для Android и кладёт её в приложение.

.DESCRIPTION
  Библиотека собирается из go\cmd\awglib тем же amneziawg-go, на котором работают
  серверы, поэтому клиент и сервер не могут разойтись в протоколе. Для сборки нужен
  Android NDK (ставится вместе с Android Studio): он даёт компилятор C, которого
  Go требует для общей библиотеки.

  По умолчанию собираются все три архитектуры, под которые Flutter собирает выпускной
  APK: arm64-v8a (почти все телефоны), armeabi-v7a (32-битные ARM, старые и недорогие
  телефоны) и x86_64 (эмулятор и немногие планшеты и Chromebook). Без библиотеки для
  какой-то из них приложение на таком устройстве не запустится: туннель в ней.
#>
param(
    [string[]]$Abis = @('arm64-v8a', 'armeabi-v7a', 'x86_64'),
    [string]$NdkVersion = '27.0.12077973',
    [int]$ApiLevel = 24
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$go = Join-Path $root 'go'
$sdk = if ($env:ANDROID_HOME) { $env:ANDROID_HOME } else { Join-Path $env:LOCALAPPDATA 'Android\Sdk' }
$ndk = Join-Path $sdk "ndk\$NdkVersion"
$bin = Join-Path $ndk 'toolchains\llvm\prebuilt\windows-x86_64\bin'
if (-not (Test-Path $bin)) { throw "Нет NDK $NdkVersion в $sdk\ndk. Доступные: $((Get-ChildItem (Join-Path $sdk 'ndk') -Directory).Name -join ', ')" }

# ABI -> (GOARCH, GOARM, clang triple). The 32-bit ARM compiler's name has the
# architecture (armv7a) and the "eabi" ending that the others do not.
$targets = @{
    'arm64-v8a'   = @{ Arch = 'arm64'; Arm = ''; Triple = 'aarch64-linux-android' }
    'armeabi-v7a' = @{ Arch = 'arm'; Arm = '7'; Triple = 'armv7a-linux-androideabi' }
    'x86_64'      = @{ Arch = 'amd64'; Arm = ''; Triple = 'x86_64-linux-android' }
}

foreach ($abi in $Abis) {
    $t = $targets[$abi]
    if (-not $t) { throw "Неизвестная архитектура $abi" }
    $outDir = Join-Path $root "app\android\app\src\main\jniLibs\$abi"
    New-Item -ItemType Directory -Force $outDir | Out-Null
    $out = Join-Path $outDir 'libawg.so'

    Write-Host "Собираю $abi…"
    $env:GOOS = 'android'
    $env:GOARCH = $t.Arch
    if ($t.Arm) { $env:GOARM = $t.Arm }
    $env:CGO_ENABLED = '1'
    $env:CC = Join-Path $bin "$($t.Triple)$ApiLevel-clang.cmd"
    Push-Location $go
    try {
        # 16 КБ — размер страницы на новых телефонах (Android 15+); на старых это ничему не мешает.
        go build -trimpath -buildmode=c-shared `
            -ldflags '-s -w -extldflags=-Wl,-z,max-page-size=16384' `
            -o $out ./cmd/awglib
        if ($LASTEXITCODE -ne 0) { throw "go build для $abi не сработал" }
    } finally {
        Pop-Location
        Remove-Item Env:GOOS, Env:GOARCH, Env:GOARM, Env:CGO_ENABLED, Env:CC -ErrorAction SilentlyContinue
    }
    # Заголовок, который делает cgo, приложению не нужен.
    Remove-Item (Join-Path $outDir 'libawg.h') -ErrorAction SilentlyContinue
    Write-Host ("  {0}: {1:N1} МБ" -f $out, ((Get-Item $out).Length / 1MB))
}
