<#
.SYNOPSIS
  Отправляет код (все ветки и теги) на запасное место, в GitLab.

.DESCRIPTION
  Токен берётся из GITLAB_TOKEN в secrets.env (или из переменной среды) и передаётся
  git только на время этой команды, в заголовке: ни в адрес удалённого репозитория,
  ни в настройки git он не попадает и на экран не выводится.

  Нужен пустой публичный проект на GitLab и токен с правом write_repository (или api).
  Адрес проекта берётся из awgctl mirror set gitlab; можно указать и -Url.

.PARAMETER Remote
  Имя удалённого репозитория в git (по умолчанию gitlab). Если его нет, он создаётся.
.PARAMETER Url
  Адрес репозитория, https://gitlab.com/владелец/имя.git. По умолчанию из состояния
  оператора (state\state.json).
#>
param(
    [string]$Remote = 'gitlab',
    [string]$Url
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot

# --- the token
$token = $env:GITLAB_TOKEN
if (-not $token) {
    $secrets = Join-Path $root 'secrets.env'
    if (Test-Path $secrets) {
        $line = Get-Content $secrets | Where-Object { $_ -match '^\s*GITLAB_TOKEN\s*=' } | Select-Object -First 1
        if ($line) { $token = ($line -replace '^\s*GITLAB_TOKEN\s*=\s*', '').Trim().Trim('"').Trim("'") }
    }
}
if (-not $token) { throw 'Нет GITLAB_TOKEN: впиши его в secrets.env (токен GitLab с правом write_repository).' }

# --- where to
if (-not $Url) {
    $statePath = Join-Path $root 'state\state.json'
    if (Test-Path $statePath) {
        $m = (Get-Content $statePath -Raw | ConvertFrom-Json).mirrors | Where-Object { $_.kind -eq 'gitlab' } | Select-Object -First 1
        if ($m) { $Url = ($m.base.TrimEnd('/')) + '/' + $m.path + '.git' }
    }
}
if (-not $Url) { throw 'Не знаю, куда отправлять: сделай awgctl mirror set gitlab -project владелец/имя или укажи -Url.' }
if ($Url -notmatch '^https://') { throw "Адрес должен быть https: $Url" }

Push-Location $root
try {
    $existing = (git remote) -contains $Remote
    if ($existing) {
        $have = (git remote get-url $Remote)
        if ($have -ne $Url) { throw "Удалённый репозиторий $Remote уже есть и ведёт на $have, а нужен $Url." }
    } else {
        git remote add $Remote $Url
        if ($LASTEXITCODE -ne 0) { throw 'git remote add не сработал' }
    }

    # Basic oauth2:<token>, as GitLab takes a token over https. Passed for this one
    # command only.
    $basic = [Convert]::ToBase64String([Text.Encoding]::ASCII.GetBytes("oauth2:$token"))
    $env:GIT_TERMINAL_PROMPT = '0'
    Write-Host "Отправляю ветки в $Url…"
    git -c "http.extraHeader=Authorization: Basic $basic" push $Remote --all
    if ($LASTEXITCODE -ne 0) { throw 'git push --all не сработал' }
    Write-Host 'Отправляю теги…'
    git -c "http.extraHeader=Authorization: Basic $basic" push $Remote --tags
    if ($LASTEXITCODE -ne 0) { throw 'git push --tags не сработал' }
    Write-Host 'Готово.'
} finally {
    Pop-Location
    Remove-Item Env:GIT_TERMINAL_PROMPT -ErrorAction SilentlyContinue
}
