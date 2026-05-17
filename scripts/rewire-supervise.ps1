# rewire-supervise.ps1 ? keep Rewire's HTTPS server alive.
#
# Runs on a 30-second loop:
#   * If the rewire.exe process is dead, relaunch it.
#   * If port 9999 has no LISTEN socket, kill the stale rewire.exe and relaunch.
# Logs to $RuntimeDir\supervisor.log; PID of the active rewire is in pid.txt.
#
# Designed to be started by Task Scheduler at logon.

param(
    [string]$RewireExe   = 'C:\Users\arushi\Rewire\bin\rewire.exe',
    [string]$WorkingDir  = 'C:\Users\arushi\Rewire',
    [string]$FrontendDir = 'C:\Users\arushi\Rewire\frontend',
    [string]$DataDir     = 'C:\Users\arushi\Rewire\data',
    [string]$CertDir     = 'C:\Certbot\live\gogillu.in',
    [int]   $Port        = 9999,
    [string]$RuntimeDir  = 'C:\Users\arushi\.copilot\session-state\abd0700f-1772-4351-81ba-707e1cdfc3db\files\rewire-runtime'
)

$ErrorActionPreference = 'Continue'
New-Item -ItemType Directory -Force -Path $RuntimeDir | Out-Null
$logFile   = Join-Path $RuntimeDir 'supervisor.log'
$stdoutLog = Join-Path $RuntimeDir 'stdout.log'
$stderrLog = Join-Path $RuntimeDir 'stderr.log'
$pidFile   = Join-Path $RuntimeDir 'pid.txt'
$adminFile = Join-Path $RuntimeDir 'admin-token.txt'

function Log($msg) {
    $ts = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
    "$ts | $msg" | Out-File -FilePath $logFile -Append -Encoding utf8
}

function Test-RewireAlive {
    $running = Get-Process -Name 'rewire' -ErrorAction SilentlyContinue
    if (-not $running) { return $false }
    $listening = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
    return ($null -ne $listening)
}

function Start-Rewire {
    if (Test-Path $adminFile) { $env:REWIRE_ADMIN = (Get-Content $adminFile -Raw).Trim() }
    # Payment provider creds. MUTUALLY EXCLUSIVE: if dodo-creds.env is
    # present, we prefer it and refuse to load rzp-creds.env. Reason: the
    # legacy rzp-creds.env holds test-mode keys, and the
    # /api/buy/rzp-test-simulate endpoint will happily mint free tokens
    # whenever rzpEnabled() is true with a test key. Loading both means
    # an attacker could bypass Dodo payment by hitting the rzp simulate
    # endpoint.
    $credsToLoad = @()
    $dodoFile = Join-Path $RuntimeDir 'dodo-creds.env'
    $rzpFile  = Join-Path $RuntimeDir 'rzp-creds.env'
    if (Test-Path $dodoFile) {
        $credsToLoad += $dodoFile
        if (Test-Path $rzpFile) {
            Log "skipped rzp-creds.env (dodo-creds.env takes precedence)"
        }
    } elseif (Test-Path $rzpFile) {
        $credsToLoad += $rzpFile
    }
    foreach ($credFile in $credsToLoad) {
        Get-Content $credFile | ForEach-Object {
            $line = $_.Trim()
            if (-not $line -or $line.StartsWith('#')) { return }
            $eq = $line.IndexOf('=')
            if ($eq -lt 1) { return }
            $k = $line.Substring(0, $eq).Trim()
            $v = $line.Substring($eq + 1).Trim()
            if ($k) { Set-Item -Path "env:$k" -Value $v }
        }
        Log "loaded creds from $(Split-Path -Leaf $credFile)"
    }
    $args = @('-port', $Port, '-frontend', $FrontendDir, '-datadir', $DataDir, '-certdir', $CertDir)
    $p = Start-Process -FilePath $RewireExe -ArgumentList $args `
        -WorkingDirectory $WorkingDir `
        -RedirectStandardOutput $stdoutLog `
        -RedirectStandardError  $stderrLog `
        -WindowStyle Hidden -PassThru
    $p.Id | Set-Content -Path $pidFile -NoNewline
    Log "started rewire pid=$($p.Id)"
}

Log "supervisor boot"

while ($true) {
    try {
        if (-not (Test-RewireAlive)) {
            Log "rewire not alive -- restarting"
            if (Test-Path $pidFile) {
                $oldPid = (Get-Content $pidFile -Raw).Trim()
                if ($oldPid -match '^\d+$') {
                    $proc = Get-Process -Id ([int]$oldPid) -ErrorAction SilentlyContinue
                    if ($proc) {
                        try { Stop-Process -Id ([int]$oldPid) -Force -ErrorAction SilentlyContinue } catch {}
                        Start-Sleep -Seconds 1
                    }
                }
            }
            Start-Rewire
        }
    } catch {
        Log "supervisor tick error: $_"
    }
    Start-Sleep -Seconds 30
}
