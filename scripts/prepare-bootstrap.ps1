<#
.SYNOPSIS
  Готовит app\assets\bootstrap.json: данные, с которыми собирается приложение.

.DESCRIPTION
  Пересобирает awgctl (чтобы он был тем, что в коде) и просит у него набор для
  сборки: ключи, адрес gist и адреса запасных мест для установщиков (awgctl mirror).
  Файл перезаписывается каждый раз: иначе добавленное запасное место не попало бы в
  приложение, которое собирается позже, и никто бы этого не заметил.

  Без состояния оператора (state\state.json), например на другом компьютере, берётся
  уже лежащий файл. Он и есть доступ, поэтому в репозиторий не попадает.
#>
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$bootstrap = Join-Path $root 'app\assets\bootstrap.json'
$awgctl = Join-Path $root 'dist\awgctl.exe'

if (-not (Test-Path (Join-Path $root 'state\state.json'))) {
    if (Test-Path $bootstrap) {
        Write-Host 'Нет состояния оператора (state\state.json): беру уже лежащий app\assets\bootstrap.json.'
        return
    }
    throw 'Нет ни app\assets\bootstrap.json, ни состояния оператора (state\state.json), из которого его сделать.'
}

if (Get-Command go -ErrorAction SilentlyContinue) {
    Push-Location (Join-Path $root 'go')
    try {
        & go build -o $awgctl ./cmd/awgctl
        if ($LASTEXITCODE -ne 0) { throw 'go build awgctl не сработал' }
    } finally {
        Pop-Location
    }
} elseif (-not (Test-Path $awgctl)) {
    throw 'Нет dist\awgctl.exe и нет Go, чтобы его собрать.'
}

& $awgctl trust -o $bootstrap
if ($LASTEXITCODE -ne 0) { throw 'awgctl trust не сработал' }

$made = Get-Content $bootstrap -Raw | ConvertFrom-Json
# @($null).Count is 1 in PowerShell, hence the tests.
$mirrors = if ($made.release_mirrors) { @($made.release_mirrors).Count } else { 0 }
$places = if ($made.config_mirrors) { @($made.config_mirrors).Count } else { 0 }
Write-Host "app\assets\bootstrap.json готов, запасных мест для установщиков: $mirrors, вторых мест для файлов конфигурации: $places."
