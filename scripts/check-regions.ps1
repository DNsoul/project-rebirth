# Проверяет, открываются ли с этого компьютера регионы хостеров: Vultr (где
# живут серверы сейчас) и кандидаты на замену с почасовой оплатой и API:
# Cherry Servers, Kamatera, UpCloud. Если регион не открывается, называет, что
# именно режут.
#
# Aeza здесь нет: у неё нет ни тестовых файлов, ни открытого сервиса в своей
# сети, с которым можно обменяться данными (только ping из её looking glass).
#
# Запуск: двойной щелчок по check-regions.cmd рядом с этим файлом.
#
# Сначала общая проверка интернета: российский сайт, зарубежный сайт, UDP за
# границу (наш VPN работает по UDP), честность DNS провайдера. Потом в каждом
# регионе тестовый хост самого хостера проверяется по слоям, снизу вверх:
#
#   ping         отвечает ли адрес вообще (многие сети ping режут, это подсказка);
#   соединение   открывается ли TCP на порт 443: пакеты пропадают или сбрасываются;
#   шифрование   проходит ли TLS-рукопожатие с настоящим именем хоста; если нет,
#                то проходит ли оно к тому же адресу под другим именем (так видна
#                блокировка по имени, SNI);
#   передача     скачиваются ли первые 4 МБ; если передача замирает, то же самое
#                пробуется обычным HTTP без шифрования: замирает и там — режут
#                по адресу хостинга, не замирает — режут шифрованный протокол.
#
# Всё идёт напрямую, мимо VPN. Адреса узнаются через DNS-over-HTTPS: если на
# компьютере включён Rebirth, обычный DNS отдаёт подставные адреса 198.18.x.x;
# настоящие адреса в туннель не направляются.
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

# Имя для проверки блокировки по SNI. Нейтральное и не российское: у части
# провайдеров рукопожатие с именами вроде ya.ru к зарубежным адресам режется
# само по себе, и сравнение ничего бы не показало.
$OtherName = 'example.com'

function Wait-Close {
    if (-not $NoPause) {
        Write-Host ''
        Read-Host 'Нажмите Enter, чтобы закрыть окно' | Out-Null
    }
}

function Get-Targets {
    $t = @(
        @{ P = 'Vultr';  City = 'Стокгольм';  Host = 'sto-se-ping.vultr.com'; Path = '/vultr.com.100MB.bin' }
        @{ P = 'Vultr';  City = 'Варшава';    Host = 'waw-pl-ping.vultr.com'; Path = '/vultr.com.100MB.bin' }
        @{ P = 'Vultr';  City = 'Франкфурт';  Host = 'fra-de-ping.vultr.com'; Path = '/vultr.com.100MB.bin' }
        @{ P = 'Vultr';  City = 'Амстердам';  Host = 'ams-nl-ping.vultr.com'; Path = '/vultr.com.100MB.bin' }
        @{ P = 'Cherry'; City = 'Шяуляй';     Host = 'speedtest.lt.cherryservers.com'; Path = '/test-100mb' }
        @{ P = 'Cherry'; City = 'Амстердам';  Host = 'speedtest.nl.cherryservers.com'; Path = '/test-100mb' }
        @{ P = 'Cherry'; City = 'Франкфурт';  Host = 'speedtest.de.cherryservers.com'; Path = '/test-100mb' }
        @{ P = 'Cherry'; City = 'Стокгольм';  Host = 'speedtest.se.cherryservers.com'; Path = '/test-100mb' }
        # Кандидаты на замену Vultr. У Kamatera свои серверы Speedtest (Ookla) на
        # порту 8080, файл любого размера отдаёт /download?size=.
        @{ P = 'Kamatera'; City = 'Стокгольм'; Host = 'speedtest.eu-st.kamatera.com.prod.hosts.ooklaserver.net'; Port = 8080; Path = '/download?size={size}' }
        @{ P = 'Kamatera'; City = 'Амстердам'; Host = 'speedtest.eu.kamatera.com.prod.hosts.ooklaserver.net'; Port = 8080; Path = '/download?size={size}' }
        @{ P = 'Kamatera'; City = 'Франкфурт'; Host = 'speedtest.eu-fr.kamatera.com.prod.hosts.ooklaserver.net'; Port = 8080; Path = '/download?size={size}' }
        # У UpCloud нет файлов для скачивания, но их API стоит в их же сети. Он
        # принимает 64 КБ и отвечает «нужна авторизация»; при заморозке ответ не
        # приходит, потому что данные застревают по дороге.
        @{ P = 'UpCloud'; City = 'Хельсинки'; Host = 'api.upcloud.com'; Path = '/1.3/account'; Mode = 'upload' }
    )
    if ($All) {
        $t += @{ P = 'Cherry'; City = 'Чикаго';   Host = 'speedtest.us.cherryservers.com'; Path = '/test-100mb' }
        $t += @{ P = 'Cherry'; City = 'Сингапур'; Host = 'speedtest.sg.cherryservers.com'; Path = '/test-100mb' }
    }
    return $t
}

# ---------------------------------------------------------------- измерения

# Настоящий адрес имени через DNS-over-HTTPS (Google, затем Cloudflare).
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
    return $null
}

# Что отдаёт DNS этого компьютера (провайдера или Rebirth).
function Resolve-System([string]$name) {
    try {
        return (Resolve-DnsName $name -Type A -DnsOnly -ErrorAction Stop |
            Where-Object Type -eq 'A' | Select-Object -First 1).IPAddress
    } catch { return $null }
}

function Test-Bogus([string]$ip) {
    return (-not $ip) -or $ip -match '^(0\.|10\.|127\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.)'
}

function Num([string]$s) {
    $v = 0.0
    [void][double]::TryParse($s, [Globalization.NumberStyles]::Float, [Globalization.CultureInfo]::InvariantCulture, [ref]$v)
    return $v
}

# Лучшее время ping из двух попыток, или -1.
function Measure-Ping([string]$ip) {
    $best = -1
    $p = New-Object System.Net.NetworkInformation.Ping
    for ($i = 0; $i -lt 2; $i++) {
        try {
            $r = $p.Send($ip, 1500)
            if ($r.Status -eq 'Success' -and ($best -lt 0 -or $r.RoundtripTime -lt $best)) { $best = [int]$r.RoundtripTime }
        } catch { }
    }
    $p.Dispose()
    return $best
}

# TCP на порт: ok (и за сколько), timeout (пакеты пропадают) или reset (сброс).
function Measure-Tcp([string]$ip, [int]$port, [int]$waitMs = 6000) {
    $c = New-Object System.Net.Sockets.TcpClient
    $sw = [Diagnostics.Stopwatch]::StartNew()
    try {
        $ar = $c.BeginConnect($ip, $port, $null, $null)
        if (-not $ar.AsyncWaitHandle.WaitOne($waitMs)) { return @{ State = 'timeout'; Ms = 0 } }
        try {
            $c.EndConnect($ar)
            return @{ State = 'ok'; Ms = [int]$sw.ElapsedMilliseconds }
        } catch {
            $code = $_.Exception.InnerException.SocketErrorCode
            if ("$code" -eq 'TimedOut') { return @{ State = 'timeout'; Ms = 0 } }
            return @{ State = 'reset'; Ms = [int]$sw.ElapsedMilliseconds }
        }
    } finally { $c.Close() }
}

# Скачивание через curl с заданным адресом имени. Возвращает код выхода curl,
# сколько пришло, прошло ли TLS-рукопожатие, скорость и код HTTP.
function Invoke-Fetch([string]$url, [string]$name, [int]$port, [string]$ip, [long]$bytes, [int]$limit, [switch]$Insecure, [string]$Upload) {
    $a = @('-s', '-o', 'NUL', '--connect-timeout', '8', '-m', "$limit",
        '-w', '%{size_download} %{time_appconnect} %{speed_download} %{http_code} %{size_upload}')
    if ($name -ne $ip) { $a += @('--resolve', "${name}:${port}:$ip") }
    if ($Upload) {
        $a += @('-X', 'POST', '-H', 'Content-Type: application/octet-stream', '--data-binary', "@$Upload")
    } elseif ($bytes -gt 0) {
        $a += @('-r', "0-$($bytes - 1)")
    }
    if ($Insecure) { $a += '-k' }
    $out = & curl.exe @a $url 2>$null
    $exit = $LASTEXITCODE
    $f = "$out".Trim() -split ' '
    return @{
        Exit  = $exit
        Got   = [long](Num $f[0])
        Tls   = (Num $f[1]) -gt 0
        Speed = Num $f[2]
        Code  = [int](Num $f[3])
        Up    = [long](Num $f[4])
    }
}

# Проходит ли TLS-рукопожатие с адресом под другим именем или вовсе без имени.
function Test-OtherName([string]$ip, [int]$port = 443) {
    $r = Invoke-Fetch "https://${OtherName}:$port/" $OtherName $port $ip 0 8 -Insecure
    if ($r.Tls) { return $true }
    $r = Invoke-Fetch "https://${ip}:$port/" $ip $port $ip 0 8 -Insecure
    return $r.Tls
}

# 64 КБ случайных байт для проверки загрузкой: это примерно 45 пакетов, больше
# порога заморозки (около 25).
$UploadSize = 64KB
function Get-UploadFile {
    $path = Join-Path ([IO.Path]::GetTempPath()) 'rebirth-check-upload.bin'
    if (-not (Test-Path $path) -or (Get-Item $path).Length -ne $UploadSize) {
        $b = New-Object byte[] $UploadSize
        (New-Object Random).NextBytes($b)
        [IO.File]::WriteAllBytes($path, $b)
    }
    return $path
}

# Ответ на запрос STUN по UDP: проходит ли UDP до этого адреса и обратно.
function Test-Udp([string]$ip, [int]$port) {
    if (-not $ip) { return $false }
    $u = New-Object System.Net.Sockets.UdpClient
    try {
        $u.Client.ReceiveTimeout = 3000
        $req = [byte[]](0, 1, 0, 0, 0x21, 0x12, 0xA4, 0x42) + [byte[]](1..12)
        for ($i = 0; $i -lt 2; $i++) {
            [void]$u.Send($req, $req.Length, $ip, $port)
            try {
                $ep = New-Object System.Net.IPEndPoint([Net.IPAddress]::Any, 0)
                $resp = $u.Receive([ref]$ep)
                if ($resp.Length -ge 20 -and $resp[0] -eq 1 -and $resp[1] -eq 1) { return $true }
            } catch { }
        }
        return $false
    } catch { return $false } finally { $u.Close() }
}

# Все слои для одного региона. Дальше проверяется только то, что нужно, чтобы
# назвать причину.
function Measure-Region($t, [long]$bytes, [int]$limit) {
    $m = @{ Ip = $t.Ip; Ping = -1; Tcp = $null; Https = $null; Other = $null; Http = $null }
    if (-not $m.Ip) { $m.Ip = Resolve-Real $t.Host }
    if (-not $m.Ip) { return $m }

    $port = if ($t.Port) { [int]$t.Port } else { 443 }
    $hostPort = if ($port -eq 443) { $t.Host } else { "$($t.Host):$port" }
    $m.Upload = $t.Mode -eq 'upload'

    $m.Ping = Measure-Ping $m.Ip
    $m.Tcp = Measure-Tcp $m.Ip $port
    if ($m.Tcp.State -ne 'ok') { return $m }

    if ($m.Upload) {
        $m.Https = Invoke-Fetch "https://$hostPort$($t.Path)" $t.Host $port $m.Ip 0 $limit -Upload (Get-UploadFile)
    } else {
        $path = $t.Path -replace '\{size\}', "$bytes"
        $m.Https = Invoke-Fetch "https://$hostPort$path" $t.Host $port $m.Ip $bytes $limit
    }
    $h = $m.Https
    if ($m.Upload -and $h.Tls -and $h.Code -eq 0 -and $h.Exit -ne 28) {
        # Сервер иногда закрывает соединение, не дочитав: ещё одна попытка.
        $m.Https = Invoke-Fetch "https://$hostPort$($t.Path)" $t.Host $port $m.Ip 0 $limit -Upload (Get-UploadFile)
        $h = $m.Https
    }
    if ($m.Upload -and $h.Tls -and $h.Exit -ne 28) { return $m }
    if (-not $m.Upload -and $h.Exit -eq 0 -and $h.Got -ge $bytes) { return $m }

    if (-not $h.Tls -and $h.Exit -ne 60) {
        $m.Other = Test-OtherName $m.Ip $port
    } elseif ($h.Tls -and $h.Exit -ne 0 -and -not $m.Upload) {
        # Тот же файл обычным HTTP: замирает ли без шифрования. Серверы Speedtest
        # отвечают на обоих протоколах на одном порту, остальные на 80.
        $small = [Math]::Min($bytes, 1MB)
        $plainPort = if ($port -eq 443) { 80 } else { $port }
        $plainHost = if ($plainPort -eq 80) { $t.Host } else { "$($t.Host):$plainPort" }
        $path = $t.Path -replace '\{size\}', "$small"
        $m.Http = Invoke-Fetch "http://$plainHost$path" $t.Host $plainPort $m.Ip $small ([Math]::Min($limit, 15))
    }
    return $m
}

# ---------------------------------------------------------------- выводы

# Что значат измерения: вердикт, цвет и причина простыми словами. Kind — род
# блокировки, по нему строится итог.
function Get-Verdict($m, [long]$bytes, [int]$limit) {
    $bad = 'НЕДОСТУПЕН'
    if (-not $m.Ip) {
        return @{ Verdict = 'НЕ ПРОВЕРЕН'; Color = 'Red'; Kind = 'nodns'; Reason = 'не удалось узнать адрес (нет интернета?)' }
    }
    if ($m.Tcp.State -eq 'timeout') {
        if ($m.Ping -lt 0) {
            return @{ Verdict = $bad; Color = 'Red'; Kind = 'ip'; Reason = 'блокировка по IP-адресу: сервер не отвечает совсем' }
        }
        return @{ Verdict = $bad; Color = 'Red'; Kind = 'tcp'; Reason = 'ping проходит, а соединения пропадают: режут TCP к этому адресу' }
    }
    if ($m.Tcp.State -eq 'reset') {
        return @{ Verdict = $bad; Color = 'Red'; Kind = 'reset'; Reason = 'соединение сразу сбрасывают' }
    }

    $h = $m.Https
    if ($h.Exit -eq 0 -and $h.Got -ge $bytes) {
        return @{ Verdict = 'ДОСТУПЕН'; Color = 'Green'; Kind = 'ok'; Reason = '{0:N0} Мбит/с' -f ($h.Speed * 8 / 1MB) }
    }
    if ($h.Exit -eq 60) {
        return @{ Verdict = $bad; Color = 'Red'; Kind = 'cert'; Reason = 'вместо сервера хостера отвечает чужой: соединение подменяют' }
    }
    if (-not $h.Tls) {
        if ($m.Other) {
            return @{ Verdict = $bad; Color = 'Red'; Kind = 'sni'; Reason = 'блокировка по имени сайта (SNI): под другим именем тот же адрес открывается' }
        }
        if ($h.Exit -eq 35 -or $h.Exit -eq 56) {
            return @{ Verdict = $bad; Color = 'Red'; Kind = 'tls'; Reason = 'шифрованное соединение (TLS) обрывают на рукопожатии' }
        }
        return @{ Verdict = $bad; Color = 'Red'; Kind = 'tls'; Reason = 'шифрованное соединение (TLS) не устанавливается: рукопожатие замирает' }
    }

    # Проверка загрузкой: важно только, дошли ли 64 КБ и пришёл ли ответ.
    if ($m.Upload) {
        if ($h.Code -gt 0) {
            return @{ Verdict = 'ДОСТУПЕН'; Color = 'Green'; Kind = 'ok'; Reason = 'передача проходит (проверено загрузкой)' }
        }
        if ($h.Exit -eq 28) {
            return @{ Verdict = $bad; Color = 'Red'; Kind = 'freeze'; Reason = 'передача замирает: 64 КБ не доходят до сервера' }
        }
        return @{ Verdict = $bad; Color = 'Red'; Kind = 'freeze'; Reason = 'соединение рвут во время передачи' }
    }

    # Рукопожатие прошло, сервер ответил, но не файлом: значит, сломалась сама
    # проверка (у хостера сменился тестовый адрес), это не блокировка.
    if ($h.Exit -eq 0 -and $h.Code -ne 200 -and $h.Code -ne 206) {
        return @{ Verdict = 'НЕ ПРОВЕРЕН'; Color = 'Yellow'; Kind = 'answer'; Reason = "сервер открылся, но вместо файла ответил ошибкой $($h.Code); это не блокировка, у хостера сменился тестовый адрес" }
    }

    # Рукопожатие прошло, но файл не докачался.
    $httpOk = $m.Http -and $m.Http.Exit -eq 0 -and ($m.Http.Code -eq 200 -or $m.Http.Code -eq 206) -and $m.Http.Got -ge [Math]::Min($bytes, 1MB)
    $httpFroze = $m.Http -and $m.Http.Got -lt 64KB -and ($m.Http.Code -eq 0 -or $m.Http.Code -eq 200 -or $m.Http.Code -eq 206)
    $kb = [Math]::Round($h.Got / 1KB)
    if ($h.Got -lt 64KB) {
        $how = if ($h.Exit -eq 56) { "соединение рвут после $kb КБ" } else { "передача замирает после $kb КБ" }
        if ($httpOk) {
            return @{ Verdict = $bad; Color = 'Red'; Kind = 'freeze-tls'; Reason = "${how}, а без шифрования (HTTP) всё проходит: режут шифрованный протокол" }
        }
        if ($httpFroze) {
            return @{ Verdict = $bad; Color = 'Red'; Kind = 'freeze'; Reason = "${how}, и без шифрования тоже: режут весь трафик к хостингу" }
        }
        return @{ Verdict = $bad; Color = 'Red'; Kind = 'freeze'; Reason = $how }
    }
    $got = Format-Size $h.Got
    $want = Format-Size $bytes
    if ($httpOk -and $m.Http.Speed -gt 4 * [Math]::Max($h.Speed, 1)) {
        return @{ Verdict = 'ПЛОХО'; Color = 'Yellow'; Kind = 'slow-tls'; Reason = "за $limit с пришло $got из ${want}; без шифрования быстро: замедляют шифрованный трафик" }
    }
    return @{ Verdict = 'ПЛОХО'; Color = 'Yellow'; Kind = 'slow'; Reason = "очень медленно: за $limit с пришло $got из $want" }
}

# Размер так, чтобы были видны и первые килобайты, и мегабайты.
function Format-Size([long]$n) {
    if ($n -lt 1MB) { return '{0:N0} КБ' -f ($n / 1KB) }
    return '{0:N1} МБ' -f ($n / 1MB)
}

# Вторая строка под регионом: что показал каждый слой.
function Format-Layers($m, [long]$bytes) {
    if (-not $m.Ip) { return '' }
    $parts = @()
    $parts += if ($m.Ping -ge 0) { "ping $($m.Ping) мс" } else { 'ping: нет ответа' }
    switch ($m.Tcp.State) {
        'ok'      { $parts += "соединение: да, $($m.Tcp.Ms) мс" }
        'timeout' { $parts += 'соединение: пакеты пропадают' }
        'reset'   { $parts += 'соединение: сброшено' }
    }
    if ($m.Https) {
        $h = $m.Https
        if ($h.Tls) { $parts += 'шифрование: да' }
        elseif ($h.Exit -eq 60) { $parts += 'шифрование: чужой сертификат' }
        else {
            $o = if ($m.Other) { "да, под именем $OtherName" } else { 'нет' }
            $parts += "шифрование: нет (другое имя: $o)"
        }
        if ($h.Tls -and $m.Upload) {
            $a = if ($h.Code -gt 0) { 'ответ получен' } else { 'ответа нет' }
            $parts += "передача: загрузка $(Format-Size $UploadSize), $a"
        } elseif ($h.Tls) {
            $parts += "передача: $(Format-Size $h.Got) из $(Format-Size $bytes)"
        }
    }
    if ($m.Http) {
        $parts += "без шифрования: $(Format-Size $m.Http.Got) из $(Format-Size ([Math]::Min($bytes, 1MB)))"
    }
    return ($parts -join ' · ')
}

# Что значит каждый род блокировки, для итога.
$KindText = @{
    'ip'         = 'Блокировка по IP-адресу: до серверов не доходят никакие пакеты.'
    'tcp'        = 'Серверы отвечают на ping, но соединения к ним пропадают: режут TCP по адресу.'
    'reset'      = 'Соединение сбрасывают сразу: так делает провайдер или сервер выключен.'
    'cert'       = 'Вместо сервера хостера отвечает кто-то другой: провайдер подменяет соединение.'
    'sni'        = 'Блокировка по имени (SNI): сам адрес открыт, режут по имени сайта в рукопожатии.'
    'tls'        = 'Соединение есть, но шифрованное рукопожатие (TLS) не проходит ни под каким именем: режут протокол к этому адресу.'
    'freeze'     = 'Соединение есть, но передача замирает после первых килобайт. Так ТСПУ режет трафик к зарубежным хостингам: по адресу хостинга, а не по протоколу.'
    'freeze-tls' = 'Без шифрования данные идут, а шифрованные замирают: режут по протоколу (TLS).'
    'slow'       = 'Не блокируют, но сильно замедляют.'
    'slow-tls'   = 'Шифрованный трафик замедляют, обычный идёт быстро.'
    'answer'     = 'Тестовый адрес хостера отвечает ошибкой: это сломалась проверка, не блокировка. Пришлите снимок, я поправлю скрипт.'
    'nodns'      ='Не удалось узнать адреса даже через зашифрованный DNS: похоже, интернета нет.'
}

# ---------------------------------------------------------------- проверка

function Write-Row([string]$label, [string]$value, [string]$color) {
    Write-Host ('  {0,-34}' -f $label) -NoNewline
    Write-Host $value -ForegroundColor $color
}

function Test-General {
    $g = @{}
    Write-Host 'Интернет в целом:'

    $ru = Resolve-Real 'ya.ru'
    $g.Russia = $ru -and ((Invoke-Fetch 'https://ya.ru/' 'ya.ru' 443 $ru 0 10).Exit -eq 0)
    if ($g.Russia) { Write-Row 'Российские сайты (ya.ru)' 'открываются' Green }
    else { Write-Row 'Российские сайты (ya.ru)' 'не открываются' Red }

    $gg = Resolve-Real 'www.google.com'
    $g.Abroad = $gg -and ((Invoke-Fetch 'https://www.google.com/' 'www.google.com' 443 $gg 0 10).Exit -eq 0)
    if ($g.Abroad) { Write-Row 'Зарубежные сайты (google.com)' 'открываются' Green }
    else { Write-Row 'Зарубежные сайты (google.com)' 'не открываются' Red }

    $cf = Test-Udp (Resolve-Real 'stun.cloudflare.com') 3478
    $hz = Test-Udp (Resolve-Real 'stun.nextcloud.com') 3478
    $g.Udp = $cf -or $hz
    $g.UdpHosting = $hz
    if ($cf -and $hz) { Write-Row 'UDP за границу (на нём наш VPN)' 'проходит' Green }
    elseif ($cf) { Write-Row 'UDP за границу (на нём наш VPN)' 'до Cloudflare проходит, до зарубежного хостинга нет' Yellow }
    elseif ($hz) { Write-Row 'UDP за границу (на нём наш VPN)' 'до хостинга проходит, до Cloudflare нет' Yellow }
    else { Write-Row 'UDP за границу (на нём наш VPN)' 'не проходит' Red }

    $probe = 'sto-se-ping.vultr.com'
    $sys = Resolve-System $probe
    $g.Rebirth = $sys -match '^198\.(18|19)\.'
    if ($g.Rebirth) { Write-Row 'DNS провайдера' 'не проверить: включён Rebirth (регионы проверяются мимо него)' DarkGray }
    elseif (Test-Bogus $sys) { Write-Row 'DNS провайдера' 'не отдаёт адреса хостеров (регионы проверяются в обход)' Yellow }
    else { Write-Row 'DNS провайдера' 'отдаёт настоящие адреса' Green }

    Write-Host ''
    return $g
}

function Main {
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        $Host.UI.RawUI.WindowTitle = 'Проверка регионов'

        if (-not (Get-Command curl.exe -ErrorAction SilentlyContinue)) {
            Write-Host 'Не могу проверить: в этой Windows нет curl.exe (он есть в Windows 10 1803 и новее).' -ForegroundColor Red
            return
        }

        $targets = Get-Targets
        $bytes = $MB * 1MB
        $limit = [Math]::Max(20, $MB * 5)   # секунд на хост
        Write-Host 'Проверяю, открываются ли наши серверные регионы с этого интернета, и если нет, то что именно режут.'
        Write-Host ("Это займёт до {0} минут. Окно само не закроется.`n" -f [Math]::Ceiling($targets.Count * ($limit + 25) / 60))

        $g = Test-General

        Write-Host 'Регионы:'
        $verdicts = @()
        foreach ($t in $targets) {
            $name = '{0,-8} {1,-10}' -f $t.P, $t.City
            Write-Host "  $name  проверяю…" -NoNewline
            $m = Measure-Region $t $bytes $limit
            $v = Get-Verdict $m $bytes $limit
            $v.Name = $name
            Write-Host ("`r  {0}  " -f $name) -NoNewline
            Write-Host ('{0,-11}' -f $v.Verdict) -ForegroundColor $v.Color -NoNewline
            Write-Host " $($v.Reason)"
            $layers = Format-Layers $m $bytes
            if ($layers) { Write-Host "                       $layers" -ForegroundColor DarkGray }
            $verdicts += $v
        }

        Write-Summary $g $verdicts
        Write-Host ''
        Write-Host 'Сделайте снимок этого окна и пришлите его, если нужно разобраться.'
    } catch {
        Write-Host ''
        Write-Host "Проверка оборвалась из-за ошибки: $($_.Exception.Message)" -ForegroundColor Red
    }
}

function Write-Summary($g, $verdicts) {
    Write-Host ''
    $ok = @($verdicts | Where-Object { $_.Kind -eq 'ok' }).Count
    $n = $verdicts.Count

    if (-not $g.Russia -and -not $g.Abroad -and $ok -eq 0) {
        Write-Host 'ИТОГ: не открывается вообще ничего. Похоже, интернета нет.' -ForegroundColor Red
        return
    }
    if ($g.Russia -and -not $g.Abroad -and $ok -eq 0) {
        Write-Host 'ИТОГ: открываются только российские сайты. Похоже на режим белых списков: всё зарубежное закрыто.' -ForegroundColor Red
        return
    }

    if ($ok -eq $n) {
        Write-Host 'ИТОГ: все регионы открываются.' -ForegroundColor Green
    } elseif ($ok -eq 0) {
        Write-Host 'ИТОГ: ни один регион не открылся.' -ForegroundColor Red
    } else {
        Write-Host ("ИТОГ: открываются {0} из {1}." -f $ok, $n) -ForegroundColor Yellow
    }

    $groups = $verdicts | Where-Object { $_.Kind -ne 'ok' } | Group-Object { $_.Kind }
    foreach ($grp in $groups) {
        $where = ($grp.Group | ForEach-Object { $_.Name.Trim() -replace '\s+', ' ' }) -join ', '
        Write-Host ''
        Write-Host "  $($KindText[$grp.Name])" -ForegroundColor Yellow
        Write-Host "  Где: $where"
    }

    if (-not $g.Udp) {
        Write-Host ''
        Write-Host '  UDP за границу не проходит вовсе. Наш VPN работает по UDP, поэтому с этого интернета он не подключится, даже если регионы выше открываются.' -ForegroundColor Red
    } elseif (-not $g.UdpHosting) {
        Write-Host ''
        Write-Host '  UDP до зарубежного хостинга не проходит. Наш VPN работает по UDP к таким серверам и может не подключиться.' -ForegroundColor Yellow
    }
}

# При подключении через точку (. .\check-regions.ps1) только объявляются
# функции: так их можно проверить по отдельности.
if ($MyInvocation.InvocationName -ne '.') {
    Main
    Wait-Close
}
