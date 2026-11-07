# Проверка разделения трафика на этом компьютере. Только читает, ничего не меняет.
#
# Запуск (из любой папки, после подключения туннеля из awg.conf):
#   powershell -ExecutionPolicy Bypass -File .\scripts\test-split.ps1

param(
    [string]$Conf = (Join-Path $PSScriptRoot '..\awg.conf')
)

$ErrorActionPreference = 'Stop'
$script:failed = 0

function Show([bool]$ok, [string]$title, [string]$detail) {
    $mark = if ($ok) { 'OK  ' } else { 'FAIL' ; $script:failed++ }
    Write-Host ("[{0}] {1}" -f $mark, $title)
    if ($detail) { Write-Host ("       {0}" -f $detail) }
}

function Resolve4([string]$name) {
    $r = Resolve-DnsName -Name $name -Type A -DnsOnly -ErrorAction Stop
    @($r | Where-Object { $_.Type -eq 'A' } | ForEach-Object { $_.IPAddress })
}

# Через какой интерфейс пойдёт пакет к этому адресу.
function RouteVia([string]$ip) {
    $r = Find-NetRoute -RemoteIPAddress $ip -ErrorAction Stop | Where-Object { $_.InterfaceAlias } | Select-Object -First 1
    $r.InterfaceAlias
}

$line = Get-Content $Conf | Where-Object { $_ -match '^\s*Endpoint\s*=' } | Select-Object -First 1
if (-not $line) { throw "В $Conf нет строки Endpoint" }
$server = (($line -split '=', 2)[1].Trim() -split ':')[0]
Write-Host "Сервер: $server"
Write-Host ''

# 0. Где туннель.
$tunnel = RouteVia '198.18.0.1'
Show ($tunnel -ne $null) 'Резолвер 198.18.0.1 достижим' "через интерфейс: $tunnel"

# 1. Зарубежный сайт должен получить подставной адрес. Если пришёл настоящий,
#    систему опросила не только наш резолвер (утечка DNS).
try {
    $foreign = Resolve4 'github.com'
    $allStandIn = ($foreign.Count -gt 0) -and (@($foreign | Where-Object { $_ -notlike '198.18.*' }).Count -eq 0)
    $detail = "github.com -> " + ($foreign -join ' ')
    if (-not $allStandIn) {
        $detail += "`n       Пришёл настоящий адрес: Windows спросила другой DNS. См. подсказку внизу."
    }
    Show $allStandIn 'Зарубежный сайт получает подставной адрес' $detail
} catch {
    Show $false 'Зарубежный сайт получает подставной адрес' "не резолвится: $($_.Exception.Message)"
    $foreign = @()
}

# 2. Этот подставной адрес должен идти в туннель.
if ($foreign.Count -gt 0) {
    $via = RouteVia $foreign[0]
    Show ($via -eq $tunnel) 'Подставной адрес идёт в туннель' "$($foreign[0]) -> $via"
}

# 3. Российский сайт должен получить настоящий адрес и идти мимо туннеля.
try {
    $local = Resolve4 'yandex.ru'
    $real = ($local.Count -gt 0) -and (@($local | Where-Object { $_ -like '198.18.*' }).Count -eq 0)
    Show $real 'Российский сайт получает настоящий адрес' ("yandex.ru -> " + ($local -join ' '))
    if ($local.Count -gt 0) {
        $via = RouteVia $local[0]
        Show ($via -ne $tunnel) 'Российский сайт идёт напрямую (мимо туннеля)' "$($local[0]) -> $via"
    }
} catch {
    Show $false 'Российский сайт получает настоящий адрес' "не резолвится: $($_.Exception.Message)"
}

# 4. Telegram ходит по зашитым адресам, без DNS: они прописаны в маршрутах.
$tg = RouteVia '149.154.167.99'
Show ($tg -eq $tunnel) 'Адреса Telegram идут в туннель' "149.154.167.99 -> $tg"

# 5. Зарубежный сайт должен видеть адрес сервера, а не домашний.
try {
    $seen = (& curl.exe -s -m 20 https://api.ipify.org).Trim()
    Show ($seen -eq $server) 'Зарубежный сайт видит адрес сервера' "api.ipify.org видит: $seen (ожидалось $server)"
} catch {
    Show $false 'Зарубежный сайт видит адрес сервера' "запрос не удался: $($_.Exception.Message)"
}

Write-Host ''
if ($script:failed -eq 0) {
    Write-Host 'Всё в порядке.'
} else {
    Write-Host "Не прошло проверок: $script:failed"
    Write-Host ''
    Write-Host 'Если провалилась проверка DNS, заставь Windows спрашивать только наш резолвер'
    Write-Host '(PowerShell от имени администратора):'
    Write-Host '  Add-DnsClientNrptRule -Namespace "." -NameServers "198.18.0.1"'
    Write-Host 'и убери правило после проверки, иначе без туннеля не будет DNS:'
    Write-Host '  Get-DnsClientNrptRule | Where-Object { $_.NameServers -eq "198.18.0.1" } | Remove-DnsClientNrptRule -Force'
}
