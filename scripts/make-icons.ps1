<#
.SYNOPSIS
  Делает значки Rebirth: четыре для области уведомлений, один для программы на
  Windows и ресурсы для Android.

.DESCRIPTION
  Сам рисунок феникса лежит в app\lib\ui\rebirth_mark.dart: тот же код рисует его
  на экране приложения и здесь, поэтому значок и приложение не могут разойтись.
  Скрипт запускает app\test\brand_test.dart, который выгружает рисунки в PNG, и
  собирает из них файлы.

  Значки в трее показывают состояние: серый диск — отключено, оранжевый — подключено,
  тёмный с янтарным кольцом — идёт работа или нет ответа, красный — ошибка.

  Размеры до 64 пикселей пишутся как обычные битовые карты (их читает любая версия
  Windows и любой код загрузки значков), 256 — как PNG.

  Запуск: .\scripts\make-icons.ps1
#>
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing

$root = Split-Path -Parent $PSScriptRoot
$app = Join-Path $root 'app'
$trayDir = Join-Path $app 'assets\tray'
$appIcon = Join-Path $app 'windows\runner\resources\app_icon.ico'
$res = Join-Path $app 'android\app\src\main\res'
$pictures = Join-Path ([IO.Path]::GetTempPath()) 'rebirth-brand'
New-Item -ItemType Directory -Force $trayDir | Out-Null

# 1. The pictures.
if (Test-Path $pictures) { Remove-Item $pictures -Recurse -Force }
Push-Location $app
try {
    $env:BRAND_DIR = $pictures
    flutter test test/brand_test.dart
    if ($LASTEXITCODE -ne 0) { throw 'flutter test не нарисовал значки' }
} finally {
    Remove-Item Env:BRAND_DIR -ErrorAction SilentlyContinue
    Pop-Location
}

function Get-Picture([string]$name) {
    $path = Join-Path $pictures "$name.png"
    if (-not (Test-Path $path)) { throw "Нет рисунка $name" }
    return $path
}

# The pixels of a picture as an icon image: a header, the colours bottom-up, and an
# empty mask (the transparency is in the colours).
function Get-DibEntry([string]$path) {
    $bmp = New-Object System.Drawing.Bitmap $path
    try {
        $w = $bmp.Width; $h = $bmp.Height
        $rect = New-Object System.Drawing.Rectangle 0, 0, $w, $h
        $data = $bmp.LockBits($rect, [System.Drawing.Imaging.ImageLockMode]::ReadOnly, [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
        $stride = $data.Stride
        $raw = New-Object byte[] ($stride * $h)
        [System.Runtime.InteropServices.Marshal]::Copy($data.Scan0, $raw, 0, $raw.Length)
        $bmp.UnlockBits($data)
    } finally { $bmp.Dispose() }

    $ms = New-Object System.IO.MemoryStream
    $bw = New-Object System.IO.BinaryWriter $ms
    $bw.Write([uint32]40); $bw.Write([int32]$w); $bw.Write([int32]($h * 2))
    $bw.Write([uint16]1); $bw.Write([uint16]32); $bw.Write([uint32]0)
    $bw.Write([uint32]($w * $h * 4)); $bw.Write([int32]0); $bw.Write([int32]0); $bw.Write([uint32]0); $bw.Write([uint32]0)
    for ($y = $h - 1; $y -ge 0; $y--) { $bw.Write($raw, $y * $stride, $w * 4) }
    $maskStride = [int]([Math]::Ceiling($w / 32.0) * 4)
    $bw.Write((New-Object byte[] ($maskStride * $h)))
    $bw.Flush()
    return , $ms.ToArray()
}

function Write-Ico([string]$path, [string]$prefix, [int[]]$sizes) {
    $images = @()
    foreach ($s in $sizes) {
        $file = Get-Picture "$prefix-$s"
        # The comma keeps the bytes together: without it PowerShell unrolls the array.
        $bytes = if ($s -ge 256) { , [IO.File]::ReadAllBytes($file) } else { Get-DibEntry $file }
        $images += [pscustomobject]@{ Size = $s; Bytes = $bytes }
    }

    $ms = New-Object System.IO.MemoryStream
    $bw = New-Object System.IO.BinaryWriter $ms
    $bw.Write([uint16]0); $bw.Write([uint16]1); $bw.Write([uint16]$images.Count)
    $offset = 6 + 16 * $images.Count
    foreach ($img in $images) {
        $dim = if ($img.Size -ge 256) { 0 } else { $img.Size }
        $bw.Write([byte]$dim); $bw.Write([byte]$dim); $bw.Write([byte]0); $bw.Write([byte]0)
        $bw.Write([uint16]1); $bw.Write([uint16]32)
        $bw.Write([uint32]$img.Bytes.Length); $bw.Write([uint32]$offset)
        $offset += $img.Bytes.Length
    }
    foreach ($img in $images) { $bw.Write($img.Bytes) }
    $bw.Flush()
    [System.IO.File]::WriteAllBytes($path, $ms.ToArray())
    Write-Host ("{0}  ({1} байт, размеры: {2})" -f $path, $ms.Length, ($sizes -join ', '))
}

# 2. Windows. Small sizes only for the tray: the system picks the one that fits.
$tray = @(16, 20, 24, 32, 40, 48)
foreach ($state in 'idle', 'connected', 'busy', 'error') {
    Write-Ico (Join-Path $trayDir "$state.ico") "tray-$state" $tray
}
# The program's own icon, which the taskbar and the title bar use.
Write-Ico $appIcon 'app' @(16, 24, 32, 48, 64, 256)

# 3. Android.
$densities = [ordered]@{ 'mdpi' = 48; 'hdpi' = 72; 'xhdpi' = 96; 'xxhdpi' = 144; 'xxxhdpi' = 192 }
foreach ($d in $densities.Keys) {
    $dir = Join-Path $res "mipmap-$d"
    New-Item -ItemType Directory -Force $dir | Out-Null
    Copy-Item (Get-Picture "android-legacy-$($densities[$d])") (Join-Path $dir 'ic_launcher.png') -Force
}
$stat = [ordered]@{ 'mdpi' = 24; 'hdpi' = 36; 'xhdpi' = 48; 'xxhdpi' = 72; 'xxxhdpi' = 96 }
foreach ($d in $stat.Keys) {
    $dir = Join-Path $res "drawable-$d"
    New-Item -ItemType Directory -Force $dir | Out-Null
    Copy-Item (Get-Picture "android-stat-$($stat[$d])") (Join-Path $dir 'ic_stat_rebirth.png') -Force
}
$nodpi = Join-Path $res 'drawable-nodpi'
New-Item -ItemType Directory -Force $nodpi | Out-Null
Copy-Item (Get-Picture 'android-foreground-432') (Join-Path $nodpi 'ic_launcher_foreground.png') -Force

# The adaptive icon: the bird over a soot-coloured ground, for phones that cut the
# icon to their own shape.
$adaptive = Join-Path $res 'mipmap-anydpi-v26'
New-Item -ItemType Directory -Force $adaptive | Out-Null
$xml = @'
<?xml version="1.0" encoding="utf-8"?>
<adaptive-icon xmlns:android="http://schemas.android.com/apk/res/android">
    <background android:drawable="@color/ic_launcher_background" />
    <foreground android:drawable="@drawable/ic_launcher_foreground" />
</adaptive-icon>
'@
[IO.File]::WriteAllText((Join-Path $adaptive 'ic_launcher.xml'), $xml, (New-Object System.Text.UTF8Encoding($false)))
$colors = @'
<?xml version="1.0" encoding="utf-8"?>
<resources>
    <color name="ic_launcher_background">#0F0B0A</color>
</resources>
'@
[IO.File]::WriteAllText((Join-Path $res 'values\ic_launcher_background.xml'), $colors, (New-Object System.Text.UTF8Encoding($false)))
Write-Host 'Android: значки и адаптивный значок записаны.'

# 4. The master picture, for the README and the installer.
$brand = Join-Path $root 'branding'
New-Item -ItemType Directory -Force $brand | Out-Null
Copy-Item (Get-Picture 'app-512') (Join-Path $brand 'rebirth-512.png') -Force
Copy-Item (Get-Picture 'app-1024') (Join-Path $brand 'rebirth-1024.png') -Force
Write-Host "Исходные рисунки: $brand"
