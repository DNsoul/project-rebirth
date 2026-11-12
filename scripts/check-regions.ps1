# Проверяет, открываются ли с этого компьютера наши регионы у наших хостеров:
# Vultr (где живут серверы сейчас) и Cherry Servers (запасной хостер).
#
# Запуск: двойной щелчок по check-regions.cmd рядом с этим файлом.
#
# В каждом регионе берётся тестовый хост самого хостера, и с него скачиваются
# первые 4 МБ напрямую, мимо VPN. Одного подключения мало: при блокировке
# соединение часто устанавливается, а передача замирает после первых килобайт.
#
# Адреса узнаются через DNS-over-HTTPS. Если на компьютере включён Rebirth,
# обычный DNS отдаёт подставные адреса 198.18.x.x, и проверка ушла бы в туннель;
# настоящие адреса в туннель не направляются, поэтому проверяется именно путь
# через провайдера.
#
# Из консоли: powershell -ExecutionPolicy Bypass -File check-regions.ps1 [-MB 10] [-All] [-NoPause]

param(
    # Сколько мегабайт скачивать с каждого хоста.
    [int]$MB = 4,
    # Заодно проверить дальние регионы Cherry (США, Сингапур).
    [switch]$All,
    # Не ждать Enter в конце (для запуска из консоли или по расписанию).
    [switch]$NoPause
)

function Wait-Close {
    if (-not $NoPause) {
        Write-Host ''
        Read-Host 'Нажмите Enter, чтобы закрыть окно' | Out-Null
    }
}

$targets = @(
    @{ P = 'Vultr';  City = 'Стокгольм';  Host = 'sto-se-ping.vultr.com'; Path = '/vultr.com.100MB.bin' }
    @{ P = 'Vultr';  City = 'Варшава';    Host = 'waw-pl-ping.vultr.com'; Path = '/vultr.com.100MB.bin' }
    @{ P = 'Vultr';  City = 'Франкфурт';  Host = 'fra-de-ping.vultr.com'; Path = '/vultr.com.100MB.bin' }
    @{ P = 'Vultr';  City = 'Амстердам';  Host = 'ams-nl-ping.vultr.com'; Path = '/vultr.com.100MB.bin' }
    @{ P = 'Cherry'; City = 'Шяуляй';     Host = 'speedtest.lt.cherryservers.com'; Path = '/test-100mb' }
    @{ P = 'Cherry'; City = 'Амстердам';  Host = 'speedtest.nl.cherryservers.com'; Path = '/test-100mb' }
    @{ P = 'Cherry'; City = 'Франкфурт';  Host = 'speedtest.de.cherryservers.com'; Path = '/test-100mb' }
    @{ P = 'Cherry'; City = 'Стокгольм';  Host = 'speedtest.se.cherryservers.com'; Path = '/test-100mb' }
)
if ($All) {
    $targets += @{ P = 'Cherry'; City = 'Чикаго';   Host = 'speedtest.us.cherryservers.com'; Path = '/test-100mb' }
    $targets += @{ P = 'Cherry'; City = 'Сингапур'; Host = 'speedtest.sg.cherryservers.com'; Path = '/test-100mb' }
}

# Настоящий адрес имени: сначала через DNS-over-HTTPS (Google, Cloudflare), в
# крайнем случае через системный DNS, если тот отдаёт не подставной адрес.
function Resolve-Real([string]$name) {
    $doh = @(
        @{ Url = "https://dns.google/resolve?name=$name&type=A"; Headers = @{} }
        @{ Url = "https://cloudflare-dns.com/dns-query?name=$name&type=A"; Headers = @{ accept = 'application/dns-json' } }
    )
    foreach ($d in $doh) {
        try {
            $r = Invoke-RestMethod -Uri $d.Url -Headers $d.Headers -TimeoutSec 8
            $ip = @($r.Answer | Where-Object { $_.type -eq 1 } | ForEach-Object { $_.data })[0]
            if ($ip) { return $ip }
        } catch { }
    }
    try {
        $ip = (Resolve-DnsName $name -Type A -ErrorAction Stop | Where-Object Type -eq 'A' | Select-Object -First 1).IPAddress
        if ($ip -and $ip -notmatch '^198\.(18|19)\.') { return $ip }
    } catch { }
    return $null
}

function Num([string]$s) {
    $v = 0.0
    [void][double]::TryParse($s, [Globalization.NumberStyles]::Float, [Globalization.CultureInfo]::InvariantCulture, [ref]$v)
    return $v
}

try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $Host.UI.RawUI.WindowTitle = 'Проверка регионов'

    if (-not (Get-Command curl.exe -ErrorAction SilentlyContinue)) {
        Write-Host 'Не могу проверить: в этой Windows нет curl.exe (он есть в Windows 10 1803 и новее).' -ForegroundColor Red
        Wait-Close
        exit 0
    }

    $bytes = $MB * 1MB
    $limit = [Math]::Max(20, $MB * 5)   # секунд на хост
    Write-Host 'Проверяю, открываются ли наши серверные регионы с этого интернета.'
    Write-Host ("Это займёт до {0} минут. Окно само не закроется.`n" -f [Math]::Ceiling($targets.Count * $limit / 60))

    $ok = 0
    $results = @()
    foreach ($t in $targets) {
        $name = '{0,-7} {1,-10}' -f $t.P, $t.City
        Write-Host "  $name  проверяю…" -NoNewline

        $ip = Resolve-Real $t.Host
        $verdict = ''; $detail = ''; $good = $false
        if (-not $ip) {
            $verdict = 'НЕ ПРОВЕРЕН'; $detail = 'не удалось узнать адрес (нет интернета?)'
        } else {
            $url = "https://$($t.Host)$($t.Path)"
            $out = & curl.exe -s -o NUL --connect-timeout 8 -m $limit -r "0-$($bytes - 1)" `
                --resolve "$($t.Host):443:$ip" -w '%{size_download} %{time_connect} %{speed_download}' $url 2>$null
            $code = $LASTEXITCODE
            $f = "$out".Trim() -split ' '
            $got = [long](Num $f[0]); $conn = Num $f[1]; $speed = Num $f[2]

            if ($code -eq 0 -and $got -ge $bytes) {
                $good = $true
                $verdict = 'ДОСТУПЕН'
                $detail = '{0:N0} Мбит/с, отклик {1} мс' -f ($speed * 8 / 1MB), [Math]::Round($conn * 1000)
            } elseif ($conn -eq 0) {
                $verdict = 'НЕДОСТУПЕН'; $detail = 'не подключается'
            } elseif ($got -lt 64KB) {
                $verdict = 'НЕДОСТУПЕН'; $detail = 'подключается, но передача замирает (похоже на блокировку)'
            } else {
                $verdict = 'ПЛОХО'; $detail = 'очень медленно: за {0} с пришло {1:N1} МБ из {2}' -f $limit, ($got / 1MB), $MB
            }
        }

        $color = if ($good) { 'Green' } elseif ($verdict -eq 'ПЛОХО') { 'Yellow' } else { 'Red' }
        Write-Host ("`r  {0}  " -f $name) -NoNewline
        Write-Host ('{0,-11}' -f $verdict) -ForegroundColor $color -NoNewline
        Write-Host " $detail"
        if ($good) { $ok++ }
        $results += $verdict
    }

    Write-Host ''
    if ($ok -eq $targets.Count) {
        Write-Host 'ИТОГ: все регионы открываются. Всё в порядке.' -ForegroundColor Green
    } elseif ($ok -eq 0) {
        Write-Host 'ИТОГ: ни один регион не открылся. Проверьте, работает ли интернет вообще.' -ForegroundColor Red
    } else {
        Write-Host ("ИТОГ: открываются {0} из {1}. Красные строки — регионы, которые с этого интернета недоступны." -f $ok, $targets.Count) -ForegroundColor Yellow
    }
    Write-Host 'Сделайте снимок этого окна и пришлите его, если нужно разобраться.'
} catch {
    Write-Host ''
    Write-Host "Проверка оборвалась из-за ошибки: $($_.Exception.Message)" -ForegroundColor Red
}
Wait-Close
