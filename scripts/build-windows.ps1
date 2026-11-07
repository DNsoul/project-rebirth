<#
.SYNOPSIS
  Собирает Rebirth для Windows в dist\rebirth-windows.

.DESCRIPTION
  По умолчанию приложение просит права администратора при запуске: без них оно
  не может ставить туннель и менять правила разрешения имён. Ключ -NoElevation
  собирает вариант без этого запроса. Он нужен только для разработки (запуск из
  обычной консоли), подключиться он не сможет.

  Данные для доступа (app\assets\bootstrap.json) берутся из awgctl. Они и есть
  доступ, поэтому в репозиторий не попадают.
#>
param(
    [switch]$NoElevation,
    [string]$OutDir = 'dist\rebirth-windows'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$app = Join-Path $root 'app'
$bootstrap = Join-Path $app 'assets\bootstrap.json'

if (-not (Test-Path $bootstrap)) {
    Write-Host 'Нет app\assets\bootstrap.json, делаю через awgctl…'
    & (Join-Path $root 'dist\awgctl.exe') trust -o $bootstrap
    if ($LASTEXITCODE -ne 0) { throw 'awgctl trust не сработал' }
}

# Whether Windows asks for administrator rights is decided when the project is
# configured, from the environment. The configuration is only redone when the
# project file changes, so it is touched to make sure a build of the other kind
# is not mistaken for this one. (Found by building both ways and looking inside
# the program.)
(Get-Item (Join-Path $app 'windows\runner\CMakeLists.txt')).LastWriteTime = Get-Date
if ($NoElevation) { $env:AWG_NO_ELEVATION = '1' } else { Remove-Item Env:AWG_NO_ELEVATION -ErrorAction SilentlyContinue }

try {
    Push-Location $app
    try {
        flutter build windows --release
        if ($LASTEXITCODE -ne 0) { throw 'flutter build не сработал' }
    } finally {
        Pop-Location
    }
} finally {
    Remove-Item Env:AWG_NO_ELEVATION -ErrorAction SilentlyContinue
}

$out = Join-Path $root $OutDir
if (Test-Path $out) { Remove-Item $out -Recurse -Force }
New-Item -ItemType Directory -Force $out | Out-Null
Copy-Item (Join-Path $app 'build\windows\x64\runner\Release\*') $out -Recurse -Force

if ($NoElevation) {
    Write-Host "Готово (БЕЗ запроса прав администратора, только для разработки): $out"
} else {
    Write-Host "Готово: $out\Rebirth.exe"
    Write-Host 'При запуске Windows спросит права администратора.'
}
