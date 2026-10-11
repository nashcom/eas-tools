<#
.SYNOPSIS
Round trip test of the whole toolbox, in one command (Windows PowerShell 5.1 or newer).

.DESCRIPTION
  1. eas-probe     one probe of the account: all its devices (and with -mail a test
                  mail to itself that is deleted again). The gate: if it fails,
                  nothing else is started
  2. coordinator  nshtestherd: a running one (-herd or HERD_URL), or the container this script starts
  3. eas-load      simulated users: probe job, directory lookups, (with -mail) mail
                  to self (round trip), a short sync; then stop
  4. checks       job results and the numbers of the worker

Without -server everything runs against the mock server on this machine (demo).
With -server it runs against a real server: your account only, small, gentle.
Mail is only created there when you ask for it (-mail); in the demo it is on.

Exit code: 0 all checks passed, 1 a check failed, 2 usage or setup problem.

.EXAMPLE
.\scripts\roundtrip.ps1

.EXAMPLE
.\scripts\roundtrip.ps1 -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file d:\nsh_lab_pwd.txt

.EXAMPLE
.\scripts\roundtrip.ps1 -server traveler.lab.dnug.eu -user nsh@lab.dnug.eu -password-file d:\nsh_lab_pwd.txt -only-probe

.PARAMETER Server        real ActiveSync server, host or URL (default: demo with the mock server)
.PARAMETER User          login of the account (real server)
.PARAMETER PasswordFile  first line is the password (real server; or set EASPROBE_PASSWORD)
.PARAMETER Herd          use this running coordinator (nshtestherd) instead of starting one; it must know the account
.PARAMETER OnlyProbe     stop after the eas-probe step
.PARAMETER Mail          create test mail (a round trip to yourself, deleted again): opt-in for a real server, on by default in the demo
.PARAMETER NoMail        no test mail (demo)
.PARAMETER ProbeWorkers  probe up to N devices at the same time in step 1 (default 1: one after the other; the first device always runs alone as the login pilot)
.PARAMETER Clients       simulated users (default: 1 real, 3 demo)
.PARAMETER Devices       devices per user (default: 1 real, 2 demo)
.PARAMETER MailCount     mails per device (default: 1 real, 2 demo)
.PARAMETER MailWaitSeconds seconds to wait for a mail to come back (default: 60 real, 20 demo)
.PARAMETER Ping          heartbeat seconds of the probe's held-Ping test (default: 0 real, 5 demo)
.PARAMETER Profile       device profile(s), comma separated, used in turn for the devices (default: iphone)
.PARAMETER Insecure      do not verify the server certificate
.PARAMETER CaFile        extra CA certificates (PEM)
.PARAMETER BinDir        directory of the programs (default: the project, or binwin-amd64, see compile.sh -windows)
.PARAMETER ShowLog       show the worker log at the end

Environment: NSHTEST_TOKEN (coordinator token), EAS_PORT, HERD_PORT, METRICS_PORT
(mock server and coordinator ports, default 18080, 18788, 19117).
#>
[CmdletBinding()]
param(
    [string]$Server = '',
    [string]$User = '',
    [Alias('password-file')][string]$PasswordFile = '',
    [string]$Herd = '',
    [Alias('only-probe')][switch]$OnlyProbe,
    [switch]$Mail,
    [Alias('no-mail')][switch]$NoMail,
    [Alias('probe-workers')][int]$ProbeWorkers = 1,
    [int]$Clients = 0,
    [int]$Devices = 0,
    [Alias('mail-count')][int]$MailCount = 0,
    [Alias('mail-wait-seconds')][int]$MailWaitSeconds = 0,
    [int]$Ping = -1,
    [string]$Profile = 'iphone',
    [switch]$Insecure,
    [Alias('ca-file')][string]$CaFile = '',
    [Alias('bin')][string]$BinDir = '',
    [switch]$ShowLog
)

# -verbose is the common parameter of PowerShell: here it means the same as in roundtrip.sh, show the worker log at the end
if ($PSBoundParameters.ContainsKey('Verbose')) { $ShowLog = [switch]$true }

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2

$demo = ($Server -eq '')
$doMail = $demo
if ($Mail) { $doMail = $true }
if ($NoMail) { $doMail = $false }
if ($demo) {
    if ($Clients -le 0) { $Clients = 3 }
    if ($Devices -le 0) { $Devices = 2 }
    if ($MailCount -le 0) { $MailCount = 2 }
    if ($MailWaitSeconds -le 0) { $MailWaitSeconds = 20 }
    if ($Ping -lt 0) { $Ping = 5 }
}
else {
    if ($Clients -le 0) { $Clients = 1 }
    if ($Devices -le 0) { $Devices = 1 }
    if ($MailCount -le 0) { $MailCount = 1 }
    if ($MailWaitSeconds -le 0) { $MailWaitSeconds = 60 }
    if ($Ping -lt 0) { $Ping = 0 }
}

function Get-Port([string]$name, [int]$default) {
    $v = [Environment]::GetEnvironmentVariable($name)
    if ($v) { return [int]$v }
    return $default
}
$easPort = Get-Port 'EAS_PORT' 18080
$herdPort = Get-Port 'HERD_PORT' 18788
$metricsPort = Get-Port 'METRICS_PORT' 19117

if (-not $BinDir) { $BinDir = Split-Path -Parent $PSScriptRoot }

$script:results = New-Object System.Collections.Generic.List[string]
$script:failed = $false
$script:procs = New-Object System.Collections.Generic.List[System.Diagnostics.Process]
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ('roundtrip-' + [Guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Path $tmp | Out-Null

function Write-Ok([string]$text) { Write-Host "[ OK ]   $text"; $script:results.Add("[ OK ]   $text") }
function Write-Fail([string]$text) { Write-Host "[ FAIL ] $text" -ForegroundColor Red; $script:results.Add("[ FAIL ] $text"); $script:failed = $true }
function Write-Step([string]$text) { Write-Host ''; Write-Host "== $text" }

function Find-Program([string]$name) {
    foreach ($c in @((Join-Path $BinDir "$name.exe"), (Join-Path $BinDir $name), (Join-Path $BinDir "bin\win-amd64\$name.exe"))) {
        if (Test-Path -LiteralPath $c -PathType Leaf) { return $c }
    }
    Write-Host "program '$name' not found in $BinDir; build it with ./compile.sh -windows or use -BinDir" -ForegroundColor Red
    exit 2
}

function ConvertTo-ArgString([string[]]$list) {
    $parts = foreach ($a in $list) {
        if ($a -eq '' -or $a -match '[\s"]') { '"' + $a.Replace('"', '\"') + '"' } else { $a }
    }
    return ($parts -join ' ')
}

# Runs a program to the end and returns its exit code and the whole output.
function Invoke-Capture([string]$exe, [string[]]$arguments) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $exe
    $psi.Arguments = ConvertTo-ArgString $arguments
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.CreateNoWindow = $true
    $p = [System.Diagnostics.Process]::Start($psi)
    $out = $p.StandardOutput.ReadToEnd()
    $err = $p.StandardError.ReadToEnd()
    $p.WaitForExit()
    return @{ Code = $p.ExitCode; Output = ($out + $err) }
}

function Start-Background([string]$exe, [string[]]$arguments, [string]$logName) {
    $p = Start-Process -FilePath $exe -ArgumentList (ConvertTo-ArgString $arguments) -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput (Join-Path $tmp "$logName.out.log") -RedirectStandardError (Join-Path $tmp "$logName.err.log")
    $script:procs.Add($p)
    return $p
}

function Test-PortInUse([int]$port) {
    $c = New-Object System.Net.Sockets.TcpClient
    try {
        $iar = $c.BeginConnect('127.0.0.1', $port, $null, $null)
        if ($iar.AsyncWaitHandle.WaitOne(300) -and $c.Connected) { return $true }
        return $false
    }
    catch { return $false }
    finally { $c.Close() }
}

function Assert-PortFree([int]$port) {
    if (Test-PortInUse $port) {
        Write-Host "port $port is in use, not starting (set EAS_PORT / HERD_PORT / METRICS_PORT)" -ForegroundColor Red
        exit 2
    }
}

function Get-LogTail([string]$name, [int]$lines) {
    $text = ''
    foreach ($suffix in 'err', 'out') {
        $f = Join-Path $tmp "$name.$suffix.log"
        if (Test-Path -LiteralPath $f) { $text += (Get-Content -LiteralPath $f -Tail $lines | Out-String) }
    }
    return $text
}

try {
    $probeExe = Find-Program 'eas-probe'
    $loadExe = ''
    $mockServer = ''
    if (-not $OnlyProbe) {
        $loadExe = Find-Program 'eas-load'
    }
    if ($demo) { $mockServer = Find-Program 'eas-mock-server' }

    # ---- the account ----
    if ($demo) {
        $serverUrl = "http://127.0.0.1:$easPort"
        $User = 'demo@example.com'
        $password = 'secret'
    }
    else {
        $serverUrl = $Server
        if (-not $User) { Write-Host '-User is required with -Server' -ForegroundColor Red; exit 2 }
        if ($PasswordFile) {
            if (-not (Test-Path -LiteralPath $PasswordFile)) { Write-Host "cannot read $PasswordFile" -ForegroundColor Red; exit 2 }
            $password = (Get-Content -LiteralPath $PasswordFile -TotalCount 1).TrimEnd("`r")
        }
        elseif ($env:EASPROBE_PASSWORD) { $password = $env:EASPROBE_PASSWORD }
        else { Write-Host 'no password: use -PasswordFile or set EASPROBE_PASSWORD' -ForegroundColor Red; exit 2 }
    }
    $env:EASPROBE_PASSWORD = $password

    $tls = @()
    if ($Insecure) { $tls += '-insecure' }
    if ($CaFile) { $tls += @('-ca-file', $CaFile) }

    # ---- demo: the mock server first, the probe needs it ----
    if ($demo) {
        Assert-PortFree $easPort
        Write-Step "mock ActiveSync server (eas-mock-server) on port $easPort"
        [void](Start-Background $mockServer @('-listen', "127.0.0.1:$easPort", '-latency', '20ms', '-mail-every', '5s') 'eas-mock-server')
        Start-Sleep -Seconds 1
    }

    # ---- 1. eas-probe: the gate ----
    Write-Step ("1. eas-probe: $User at $serverUrl, $Devices device(s)" + $(if ($doMail) { ', with a test mail' } else { '' }))
    $mailArgs = @()
    if ($doMail) { $mailArgs = @('-mail', '-mail-wait-seconds', "$MailWaitSeconds") }
    $probe = Invoke-Capture $probeExe (@('-server', $serverUrl, '-user', $User, '-profile', $Profile, '-devices', "$Devices", '-workers', "$ProbeWorkers", '-ping', "$Ping",
            '-state-file', (Join-Path $tmp 'probe-state.json')) + $mailArgs + $tls)
    if ($probe.Code -eq 0) {
        # compact: the header and the step table; the details when something is wrong
        $probe.Output -split "`r?`n" | Where-Object { $_ -match '^(eas-probe|=====|\d+ of \d+ devices|  (server|product|versions|policy|device) )|^  [a-z]+ +(ok|skipped)' } |
            ForEach-Object { Write-Host "   $_" }
        Write-Ok 'eas-probe: all steps ok'
    }
    else {
        $probe.Output -split "`r?`n" | ForEach-Object { Write-Host "   $_" }
        Write-Fail "eas-probe failed (exit $($probe.Code)): nothing else is started, fix this first"
        Write-Host ''
        Write-Host 'round trip FAILED at the probe'
        exit 1
    }

    if ($OnlyProbe) {
        Write-Host ''
        Write-Host "[ OK ]   round trip: probe only, passed"
        return
    }

    # ---- 2. the coordinator ----
    if (-not $Herd) { $Herd = $env:HERD_URL }
    if (-not $Herd) {
        # No coordinator given: start the real nshtestherd as a container (image nshtestherd:latest, or
        # NSHTESTHERD_IMAGE). It is removed again at the end.
        if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
            Write-Host 'no coordinator: give a running nshtestherd with -Herd URL (or HERD_URL), or install Docker so that this script can start the nshtestherd container' -ForegroundColor Red
            exit 2
        }
        Assert-PortFree $herdPort
        $Herd = "http://127.0.0.1:$herdPort"
        $image = if ($env:NSHTESTHERD_IMAGE) { $env:NSHTESTHERD_IMAGE } else { 'nshtestherd:latest' }
        $script:herdContainer = "eas-roundtrip-herd-$PID"
        Write-Step "2. coordinator: nshtestherd container on port $herdPort"
        if ($demo) {
            & docker run -d --rm --name $script:herdContainer -p "127.0.0.1:${herdPort}:8788" $image --bind 0.0.0.0 --generate "$($Clients + 2)" --password $password | Out-Null
        }
        else {
            $short = $User.Split('@')[0]
            $csv = Join-Path $tmp 'account.csv'
            [System.IO.File]::WriteAllText($csv, "Round,Trip,$password,$short,$User`n", (New-Object System.Text.UTF8Encoding($false)))
            & docker run -d --rm --name $script:herdContainer -p "127.0.0.1:${herdPort}:8788" -v "${csv}:/users.csv:ro" $image --bind 0.0.0.0 --csv /users.csv | Out-Null
            if ($Clients -gt 1) {
                Write-Host '   note: the coordinator has one account (yours): -Clients becomes 1'
                $Clients = 1
            }
        }
        if ($LASTEXITCODE -ne 0) { exit 2 }
        for ($i = 0; $i -lt 50; $i++) {
            try { [void](Invoke-WebRequest -Uri "$Herd/health" -UseBasicParsing -TimeoutSec 1); break } catch { Start-Sleep -Milliseconds 200 }
        }
    }
    else {
        Write-Step "2. coordinator: $Herd (existing)"
    }

    $headers = @{ Accept = 'application/json' }
    if ($env:NSHTEST_TOKEN) { $headers['Authorization'] = "Bearer $($env:NSHTEST_TOKEN)" }

    function Get-HerdStatus { return Invoke-RestMethod -Uri "$Herd/status" -Headers $headers -TimeoutSec 10 }
    function Get-Jobs([string]$key) {
        try { return [int](Get-HerdStatus).$key } catch { return 0 }
    }
    function Wait-Until([int]$seconds, [string]$what, [scriptblock]$condition) {
        $end = (Get-Date).AddSeconds($seconds)
        while ((Get-Date) -lt $end) {
            try { if (& $condition) { return $true } } catch { }
            Start-Sleep -Milliseconds 200
        }
        Write-Fail "timed out waiting for $what"
        return $false
    }

    try { [void](Invoke-RestMethod -Uri "$Herd/health" -TimeoutSec 10) }
    catch {
        Write-Fail "coordinator $Herd does not answer"
        exit 1
    }
    Write-Ok 'coordinator answers'

    # ---- 3. the worker ----
    Assert-PortFree $metricsPort
    Write-Step "3. eas-load: $Clients user(s) x $Devices device(s), profile $Profile"
    $load = Start-Background $loadExe (@('-herd', $Herd, '-server', $serverUrl, '-clients', "$Clients", '-devices', "$Devices",
            '-profile', $Profile, '-name', 'roundtrip', '-poll', '300ms', '-ramp', '50ms', '-report', '0',
            '-metrics', "127.0.0.1:$metricsPort", '-state-file', (Join-Path $tmp 'load-state.json')) + $tls) 'eas-load'

    if (Wait-Until 40 "all $Clients client(s) to register and be idle" { (Get-Jobs 'clients_idle') -eq $Clients }) {
        Write-Ok "$Clients client(s) registered, idle"
    }
    else {
        Write-Host (Get-LogTail 'eas-load' 15)
        exit 1
    }

    function Get-Metrics { return (Invoke-WebRequest -Uri "http://127.0.0.1:$metricsPort/metrics" -UseBasicParsing -TimeoutSec 10).Content }
    function Get-Metric([string]$pattern) {
        $m = [regex]::Match((Get-Metrics), '(?m)^' + $pattern + '\s+([0-9.eE+-]+)')
        if ($m.Success) { return [double]::Parse($m.Groups[1].Value, [Globalization.CultureInfo]::InvariantCulture) }
        return -1
    }

    function Invoke-Job([string]$job, [string]$params, [int]$limit, [string]$what) {
        $ok0 = Get-Jobs 'jobs_ok'
        $failed0 = Get-Jobs 'jobs_failed'
        $body = @{ target = 'all'; command = 'run'; job = $job }
        # the coordinator rejects an empty params field: leave it out
        if ($params) { $body['params'] = $params }
        [void](Invoke-RestMethod -Method Post -Uri "$Herd/command" -Headers $headers -Body $body -ContentType 'application/x-www-form-urlencoded' -TimeoutSec 10)

        $end = (Get-Date).AddSeconds($limit)
        while ((Get-Date) -lt $end) {
            if ((Get-Jobs 'jobs_failed') -gt $failed0) {
                Write-Fail "${what}: the job failed (worker log below)"
                Write-Host (Get-LogTail 'eas-load' 8)
                return $false
            }
            if (((Get-Jobs 'jobs_ok') - $ok0) -ge $Clients) {
                Write-Ok $what
                return $true
            }
            Start-Sleep -Milliseconds 200
        }
        Write-Fail "${what}: not finished after ${limit}s"
        return $false
    }

    # ---- 4. the jobs ----
    Write-Step '4. jobs'
    [void](Invoke-Job 'eas-probe' '' 90 'eas-probe: login, provisioning, folders, sync on every device')
    [void](Invoke-Job 'eas-gal' 'think=0.5&duration=4' 60 'eas-gal: directory lookups of the own user')

    $found = Get-Metric 'eas_load_directory_self_total\{result="found"\}'
    $missing = Get-Metric 'eas_load_directory_self_total\{result="missing"\}'
    if ($found -ge 1 -and $missing -le 0) { Write-Ok "own user found in the directory $([int]$found) times" }
    else { Write-Fail "own user in the directory: found '$found', missing '$missing'" }

    if ($doMail) {
        $mailTimeout = 60 + $MailCount * ($MailWaitSeconds + 5)
        [void](Invoke-Job 'eas-mail' "to=self&think=1-2&count=$MailCount&wait=$MailWaitSeconds&cleanup=yes" $mailTimeout `
                "eas-mail: $MailCount mail(s) per device to self, round trip")

        $sent = Get-Metric 'eas_load_mail_sent_total\{to="self"\}'
        $arrived = Get-Metric 'eas_load_mail_arrival_seconds_count'
        $lost = Get-Metric 'eas_load_mail_lost_total'
        $sum = Get-Metric 'eas_load_mail_arrival_seconds_sum'
        $want = $Clients * $Devices * $MailCount
        if ($sent -ge $want -and $arrived -eq $sent -and $lost -eq 0) {
            $avg = if ($arrived -gt 0) { '{0:N2}' -f ($sum / $arrived) } else { '0' }
            Write-Ok "mail: $([int]$sent) sent, $([int]$arrived) arrived, none lost, average round trip ${avg}s"
        }
        else { Write-Fail "mail: sent '$sent' (want $want), arrived '$arrived', lost '$lost'" }
    }
    else { Write-Host '  --    eas-mail: skipped (mail is created only with -Mail)' }

    [void](Invoke-Job 'eas-sync' 'think=1&duration=4' 60 'eas-sync: keep the mailboxes in sync')

    $metricsText = Get-Metrics
    $errors = 0
    foreach ($m in [regex]::Matches($metricsText, '(?m)^eas_load_requests_total\{op="[A-Za-z]+",result="error"\}\s+(\d+)')) { $errors += [int]$m.Groups[1].Value }
    if ($errors -eq 0) { Write-Ok 'no failed requests' }
    else { Write-Fail "$errors failed request(s): see eas_load_http_status_total / the worker log" }
    $refused = 0
    foreach ($m in [regex]::Matches($metricsText, '(?m)^eas_load_failures_total\{class="(auth|forbidden)"\}\s+(\d+)')) { $refused += [int]$m.Groups[2].Value }
    if ($refused -gt 0) { Write-Fail "$refused login(s) were refused: check password, lockouts, block lists" }

    # ---- 5. stop ----
    Write-Step '5. stop'
    [void](Invoke-RestMethod -Method Post -Uri "$Herd/command" -Headers $headers -Body @{ target = 'all'; command = 'stop' } `
            -ContentType 'application/x-www-form-urlencoded' -TimeoutSec 10)
    if (Wait-Until 30 'the worker to end' { $load.Refresh(); $load.HasExited }) { Write-Ok 'worker ended after stop' }

    if ($ShowLog) {
        Write-Host '--- worker log ---'
        Write-Host (Get-LogTail 'eas-load' 40)
    }

    Write-Host ''
    Write-Host '== result'
    $script:results | ForEach-Object { Write-Host $_ }
    if (-not $script:failed) { Write-Host "[ OK ]   round trip passed" }
    else {
        Write-Host 'round trip FAILED (use -ShowLog for the worker log)'
        exit 1
    }
}
finally {
    if ($script:herdContainer) { & docker rm -f $script:herdContainer 2>&1 | Out-Null }
    foreach ($p in $script:procs) {
        try { if (-not $p.HasExited) { $p.Kill() } } catch { }
    }
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
exit 0
