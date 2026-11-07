<#
.SYNOPSIS
  Создаёт ключ, которым подписываются релизные APK: state\android-signing\.

.DESCRIPTION
  Делается один раз и на всю жизнь приложения. Android ставит обновление поверх
  установленного приложения, только если оно подписано тем же ключом. Потерянный
  ключ означает, что у всех, кто уже поставил приложение, обновление не встанет:
  придётся удалять и ставить заново. Поэтому папка state\ (в ней и остальные
  ключи) должна лежать в резервной копии вне этого компьютера.

  Пароли случайные, на экран не выводятся и лежат только в
  state\android-signing\key.properties рядом с ключом. Папка state\ в репозиторий
  не попадает.

  Существующий ключ не перезаписывается.
#>
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$dir = Join-Path $root 'state\android-signing'
$store = Join-Path $dir 'rebirth-release.jks'
$props = Join-Path $dir 'key.properties'

if ((Test-Path $store) -or (Test-Path $props)) {
    throw "Ключ уже есть: $dir. Новый сделать нельзя: у всех, кто поставил приложение, обновление перестанет вставать."
}

function Find-Keytool {
    $onPath = Get-Command keytool.exe -ErrorAction SilentlyContinue
    if ($onPath) { return $onPath.Source }
    foreach ($c in @(
            "$env:ProgramFiles\Android\Android Studio\jbr\bin\keytool.exe",
            "$env:JAVA_HOME\bin\keytool.exe")) {
        if ($c -and (Test-Path $c)) { return $c }
    }
    throw 'Не найден keytool: он входит в Android Studio (jbr\bin) и в любой JDK.'
}
$keytool = Find-Keytool

function New-Secret {
    $bytes = New-Object byte[] 24
    [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    [Convert]::ToBase64String($bytes).Replace('+', 'x').Replace('/', 'y').Replace('=', 'z')
}
$password = New-Secret

New-Item -ItemType Directory -Force $dir | Out-Null
# The password goes to keytool through the environment, not the command line,
# where any program on the machine can read it.
$env:REBIRTH_KEY_PASSWORD = $password
# keytool reports what it is doing on stderr, and Windows PowerShell 5.1 turns any
# stderr line of a program into an error that stops the script.
$before = $ErrorActionPreference
$ErrorActionPreference = 'Continue'
try {
    & $keytool -genkeypair -keystore $store -storetype PKCS12 `
        -alias rebirth -keyalg RSA -keysize 4096 -validity 36500 `
        -dname 'CN=Rebirth' `
        -storepass:env REBIRTH_KEY_PASSWORD -keypass:env REBIRTH_KEY_PASSWORD
    $code = $LASTEXITCODE
} finally {
    $ErrorActionPreference = $before
    Remove-Item Env:REBIRTH_KEY_PASSWORD -ErrorAction SilentlyContinue
}
if ($code -ne 0 -or -not (Test-Path $store)) { throw 'keytool не справился.' }

$lines = @(
    'storeFile=rebirth-release.jks',
    "storePassword=$password",
    'keyAlias=rebirth',
    "keyPassword=$password"
)
[IO.File]::WriteAllText($props, ($lines -join "`n") + "`n", (New-Object Text.UTF8Encoding($false)))

Write-Host ''
Write-Host "Ключ создан: $store"
Write-Host 'Сохрани папку state\ в резервную копию вне этого компьютера: без этого ключа'
Write-Host 'уже выданные APK нельзя будет обновить.'
