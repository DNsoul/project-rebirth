<#
.SYNOPSIS
  Собирает службу туннеля для Windows: dist\tunnel\awgtunnel.exe и wintun.dll.

.DESCRIPTION
  Приложение везёт туннель с собой и не требует ставить AmneziaWG для Windows.
  Службу (go\cmd\awgtunnel) собирает Go из библиотеки amneziawg-windows, той самой,
  на которой работает клиент AmneziaWG. Нужен Go (версия из go\go.mod).

  wintun.dll (адаптер, через который идёт трафик) берётся готовым и подписанным из
  официального архива wintun-<версия>.zip с wintun.net. Архив проверяется по SHA-256,
  файл кладётся без изменений: лицензия Wintun (wintun-LICENSE.txt рядом) разрешает
  поставлять его вместе с программой, которая пользуется им только через его
  интерфейс, но не разрешает его менять.

  Результат складывается в dist\tunnel; scripts\build-windows.ps1 копирует его в
  папку tunnel рядом с Rebirth.exe.
#>
param(
    [string]$OutDir = 'dist\tunnel'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot

# The same archive, with the same hash, as amneziawg-windows builds its client with.
$wintunVersion = '0.14.1'
$wintunSha256 = '07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51'
$wintunUrl = "https://www.wintun.net/builds/wintun-$wintunVersion.zip"

$out = Join-Path $root $OutDir
New-Item -ItemType Directory -Force $out | Out-Null

# --- Wintun
$zip = Join-Path $out "wintun-$wintunVersion.zip"
function Test-Zip {
    (Test-Path $zip) -and ((Get-FileHash $zip -Algorithm SHA256).Hash.ToLower() -eq $wintunSha256)
}
if (-not (Test-Zip)) {
    Write-Host "Скачиваю $wintunUrl"
    Invoke-WebRequest -Uri $wintunUrl -OutFile $zip -UseBasicParsing
    if (-not (Test-Zip)) {
        Remove-Item $zip -Force -ErrorAction SilentlyContinue
        throw "Архив Wintun не прошёл проверку SHA-256 (ожидалось $wintunSha256)."
    }
}
$unpacked = Join-Path $out "wintun-$wintunVersion"
if (Test-Path $unpacked) { Remove-Item $unpacked -Recurse -Force }
Expand-Archive -Path $zip -DestinationPath $unpacked -Force
Copy-Item (Join-Path $unpacked 'wintun\bin\amd64\wintun.dll') (Join-Path $out 'wintun.dll') -Force
Copy-Item (Join-Path $unpacked 'wintun\LICENSE.txt') (Join-Path $out 'wintun-LICENSE.txt') -Force
Remove-Item $unpacked -Recurse -Force

# --- the tunnel service
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'Не найден Go. Установи его: https://go.dev/dl/'
}
$exe = Join-Path $out 'awgtunnel.exe'
$saved = @{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED }
try {
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    Push-Location (Join-Path $root 'go')
    try {
        & go build -trimpath -ldflags '-s -w' -o $exe ./cmd/awgtunnel
        if ($LASTEXITCODE -ne 0) { throw 'go build не сработал' }
    } finally {
        Pop-Location
    }
} finally {
    foreach ($k in $saved.Keys) {
        if ($null -eq $saved[$k]) { Remove-Item "Env:$k" -ErrorAction SilentlyContinue } else { Set-Item "Env:$k" $saved[$k] }
    }
}

# A build that cannot even say its version is not worth packing.
$said = & $exe version
if ($LASTEXITCODE -ne 0) { throw "$exe не запускается." }

Write-Host "Готово: $out"
foreach ($f in 'awgtunnel.exe', 'wintun.dll', 'wintun-LICENSE.txt') {
    $p = Join-Path $out $f
    Write-Host ("  {0,-20} {1,8:N0} Б  sha256 {2}" -f $f, (Get-Item $p).Length, (Get-FileHash $p -Algorithm SHA256).Hash.ToLower().Substring(0, 16))
}
Write-Host "  $said"
