# dodo-bootstrap.ps1 ? one-time setup for Rewires Dodo Payments integration.
#
# Reads the users freshly-generated API key from
#   .dodo-bootstrap-key.txt
# and (optionally) the env mode (live|test) from
#   .dodo-bootstrap-env.txt
# in the repo root.
#
# Then, via the Dodo Payments REST API:
#   1. Ensures a "Rewire Premium Lifetime" product exists (Rs.9 INR,
#      one-time, digital). Idempotent -- reuses any existing product
#      with the same name.
#   2. Ensures a webhook to
#      https://rewire.gogillu.live/api/buy/dodo-webhook exists, filtered
#      to payment.succeeded. Idempotent -- reuses any existing webhook
#      with the same URL.
#   3. Fetches the webhook signing secret.
#   4. Writes a freshly-stamped dodo-creds.env into the runtime dir:
#        REWIRE_DODO_ENV=...
#        REWIRE_DODO_API_KEY=...
#        REWIRE_DODO_WEBHOOK_SECRET=whsec_...
#        REWIRE_DODO_PRODUCT_ID=pdt_...
#   5. Prints next steps (stop running rewire so the supervisor
#      relaunches it with the new env).
#
# The script NEVER logs the api key or webhook secret. The
# bootstrap-key file is deleted at the end to reduce blast radius.
#
# Run from the repo root:
#   pwsh .\scripts\dodo-bootstrap.ps1

[CmdletBinding()]
param(
    [string]$RepoRoot   = '',
    [string]$RuntimeDir = 'C:\Users\arushi\.copilot\session-state\abd0700f-1772-4351-81ba-707e1cdfc3db\files\rewire-runtime',
    [string]$WebhookURL = 'https://rewire.gogillu.live/api/buy/dodo-webhook',
    [string]$ProductName = 'Rewire Premium Lifetime',
    [int]   $PricePaise = 900,
    [switch]$SkipDeploy
)

$ErrorActionPreference = 'Stop'

# $PSScriptRoot is unreliable inside param() defaults under
# "powershell.exe -File" invocation, so resolve RepoRoot here instead.
if (-not $RepoRoot) {
    if ($PSScriptRoot) {
        $RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
    } else {
        $RepoRoot = 'C:\Users\arushi\Rewire'
    }
}

# Windows PowerShell 5.1 defaults to SSL3/TLS 1.0 which modern APIs
# (including Dodo Payments) reject. Force TLS 1.2.
try {
    [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor [System.Net.SecurityProtocolType]::Tls12
} catch {}

# --- read inputs ---
$keyFile = Join-Path $RepoRoot '.dodo-bootstrap-key.txt'
$envFile = Join-Path $RepoRoot '.dodo-bootstrap-env.txt'

if (-not (Test-Path $keyFile)) {
    Write-Host "ERROR: $keyFile not found." -ForegroundColor Red
    Write-Host "Create it with your Dodo Payments API key (single line, no quotes)."
    Write-Host "Generate one at: https://app.dodopayments.com -> Developer -> API Keys -> Add API Key."
    exit 1
}

$apiKey = (Get-Content $keyFile -Raw).Trim()
if (-not $apiKey) {
    Write-Host "ERROR: bootstrap key file is empty." -ForegroundColor Red
    exit 1
}

$envMode = 'live'
if (Test-Path $envFile) {
    $envMode = ((Get-Content $envFile -Raw).Trim().ToLowerInvariant())
    if ($envMode -notin @('live','test')) {
        Write-Host "ERROR: env file must say 'live' or 'test' (got '$envMode')." -ForegroundColor Red
        exit 1
    }
}

$apiBase = if ($envMode -eq 'test') { 'https://test.dodopayments.com' } else { 'https://live.dodopayments.com' }
Write-Host "-> env mode: $envMode" -ForegroundColor Cyan
Write-Host "-> api host: $apiBase" -ForegroundColor Cyan
Write-Host "-> webhook url: $WebhookURL" -ForegroundColor Cyan

# Per the user's "tasks_and_progress" rules, never echo the API key.

$headers = @{
    'Authorization' = "Bearer $apiKey"
    'Content-Type'  = 'application/json'
}

function Invoke-Dodo {
    param(
        [Parameter(Mandatory)] [string]$Method,
        [Parameter(Mandatory)] [string]$Path,
        [object]$Body
    )
    $uri = "$apiBase$Path"
    $params = @{
        Uri     = $uri
        Method  = $Method
        Headers = $headers
        ErrorAction = 'Stop'
    }
    if ($PSBoundParameters.ContainsKey('Body')) {
        $params.Body = ($Body | ConvertTo-Json -Depth 8 -Compress)
    }
    try {
        return Invoke-RestMethod @params
    } catch {
        $resp = $_.Exception.Response
        $body = ''
        if ($resp) {
            try {
                $r = New-Object System.IO.StreamReader($resp.GetResponseStream())
                $body = $r.ReadToEnd()
            } catch {}
        }
        Write-Host "Dodo API $Method $Path failed: $($_.Exception.Message)" -ForegroundColor Red
        if ($body) { Write-Host "  body: $body" -ForegroundColor DarkRed }
        throw
    }
}

# --- 1. find or create product ---
Write-Host "-> looking for existing product '$ProductName'..." -ForegroundColor Cyan
$products = Invoke-Dodo -Method GET -Path '/products?page_size=100'
# Dodo paginates with a top-level `items` array (standard for their public API).
$items = if ($products.items) { $products.items } else { $products }
$existing = $items | Where-Object { $_.name -eq $ProductName } | Select-Object -First 1

if ($existing) {
    $productId = $existing.product_id
    if (-not $productId) { $productId = $existing.id }
    Write-Host "  found existing product: $productId" -ForegroundColor Green
} else {
    Write-Host "  creating product..." -ForegroundColor Cyan
    $productBody = @{
        name = $ProductName
        description = 'Lifetime access to Rewire Premium -- Intense Mode, leaderboards, more endings.'
        tax_category = 'digital_products'
        price = @{
            type = 'one_time_price'
            currency = 'INR'
            price = $PricePaise
            discount = 0
            purchasing_power_parity = $false
        }
    }
    $created = Invoke-Dodo -Method POST -Path '/products' -Body $productBody
    $productId = $created.product_id
    if (-not $productId) { $productId = $created.id }
    if (-not $productId) {
        Write-Host "ERROR: product create returned no id. Raw response:" -ForegroundColor Red
        $created | ConvertTo-Json -Depth 8 | Write-Host
        exit 1
    }
    Write-Host "  created product: $productId" -ForegroundColor Green
}

# --- 2. find or create webhook ---
Write-Host "-> looking for existing webhook with URL=$WebhookURL ..." -ForegroundColor Cyan
$webhooks = Invoke-Dodo -Method GET -Path '/webhooks?page_size=100'
$items = if ($webhooks.items) { $webhooks.items } else { $webhooks }
$existingHook = $items | Where-Object { $_.url -eq $WebhookURL } | Select-Object -First 1

if ($existingHook) {
    $webhookId = $existingHook.id
    Write-Host "  found existing webhook: $webhookId" -ForegroundColor Green
} else {
    Write-Host "  creating webhook..." -ForegroundColor Cyan
    $webhookBody = @{
        url = $WebhookURL
        description = 'Rewire -- premium token issuance'
        filter_types = @('payment.succeeded')
        disabled = $false
    }
    $hook = Invoke-Dodo -Method POST -Path '/webhooks' -Body $webhookBody
    $webhookId = $hook.id
    if (-not $webhookId) {
        Write-Host "ERROR: webhook create returned no id. Raw response:" -ForegroundColor Red
        $hook | ConvertTo-Json -Depth 8 | Write-Host
        exit 1
    }
    Write-Host "  created webhook: $webhookId" -ForegroundColor Green
}

# --- 3. fetch webhook signing secret ---
Write-Host "-> fetching webhook signing secret..." -ForegroundColor Cyan
$secretResp = Invoke-Dodo -Method GET -Path "/webhooks/$webhookId/secret"
$webhookSecret = $secretResp.secret
if (-not $webhookSecret) {
    Write-Host "ERROR: webhook secret missing in response." -ForegroundColor Red
    exit 1
}
# Don't print the secret. Just confirm we got something whsec_-shaped.
if ($webhookSecret -notlike 'whsec_*') {
    Write-Host "  warning: secret does not start with 'whsec_' (got prefix '$($webhookSecret.Substring(0,6)))." -ForegroundColor Yellow
} else {
    Write-Host "  secret retrieved (whsec_*)." -ForegroundColor Green
}

# --- 4. write dodo-creds.env into the runtime dir ---
New-Item -ItemType Directory -Force -Path $RuntimeDir | Out-Null
$credsPath = Join-Path $RuntimeDir 'dodo-creds.env'
$creds = @"
# Auto-generated by dodo-bootstrap.ps1 on $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')
# DO NOT COMMIT. Gitignored at the repo root.
REWIRE_DODO_ENV=$envMode
REWIRE_DODO_API_KEY=$apiKey
REWIRE_DODO_WEBHOOK_SECRET=$webhookSecret
REWIRE_DODO_PRODUCT_ID=$productId
"@
# Write ASCII to dodge BOM problems with environments that parse env files
# strictly (the supervisor PS1 reads UTF-8 anyway, but ASCII is safer).
[System.IO.File]::WriteAllText($credsPath, $creds, [System.Text.Encoding]::ASCII)
Write-Host "-> wrote $credsPath" -ForegroundColor Green

# --- 5. wipe the one-time bootstrap key file ---
Remove-Item $keyFile -Force
Write-Host "-> wiped $keyFile (single-use)." -ForegroundColor Green

# --- 6. swap binary, kick supervisor, verify ---
if ($SkipDeploy) {
    Write-Host ""
    Write-Host "DONE (-SkipDeploy). Manual next steps:" -ForegroundColor Green
    Write-Host "  Stop-Process -Id (Get-Content '$RuntimeDir\pid.txt')" -ForegroundColor DarkGray
    Write-Host "  curl.exe -sk https://127.0.0.1:9999/api/version" -ForegroundColor DarkGray
    Write-Host "Expected: dodo_enabled=true, dodo_env=$envMode, dodo_product_id=$productId" -ForegroundColor White
    exit 0
}

Write-Host ""
Write-Host "=== DEPLOY PHASE ===" -ForegroundColor Cyan

$rewireExe    = Join-Path $RepoRoot 'bin\rewire.exe'
$rewireNewExe = Join-Path $RepoRoot 'bin\rewire-new.exe'
$pidFile      = Join-Path $RuntimeDir 'pid.txt'

# The supervisor (RewireSupervisor scheduled task) was launched at boot
# and has the OLD supervise.ps1 in memory. We've since edited it to
# load dodo-creds.env. Restart the scheduled task so the new logic
# takes effect. (The task launches a fresh powershell.exe each time.)
Write-Host "-> restarting RewireSupervisor scheduled task ..." -ForegroundColor Cyan
$endResult = schtasks.exe /End /TN RewireSupervisor 2>&1
Write-Host "  end: $endResult"
Start-Sleep -Seconds 2
# Also kill any leftover powershell.exe running supervise.ps1 (in case
# /End didn't fully clean up).
Get-CimInstance Win32_Process -Filter "Name='powershell.exe'" -ErrorAction SilentlyContinue | Where-Object {
    $_.CommandLine -and $_.CommandLine -like '*rewire-supervise.ps1*'
} | ForEach-Object {
    Write-Host "  killing stale supervisor pid=$($_.ProcessId)"
    try { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue } catch {}
}
Start-Sleep -Seconds 1

# Stop the running rewire so the .exe becomes unlocked for the swap. (The
# old supervisor is now gone so it won't auto-relaunch.)
$wasRunning = $false
if (Test-Path $pidFile) {
    $oldPid = (Get-Content $pidFile -Raw).Trim()
    if ($oldPid -match '^\d+$') {
        try {
            $proc = Get-Process -Id $oldPid -ErrorAction Stop
            $wasRunning = $true
            Write-Host "-> stopping rewire pid=$oldPid ..." -ForegroundColor Cyan
            Stop-Process -Id $oldPid -Force
            Start-Sleep -Seconds 2
        } catch {
            Write-Host "  pid $oldPid not running" -ForegroundColor DarkGray
        }
    }
}

# Swap in the freshly-built binary if it's newer.
if (Test-Path $rewireNewExe) {
    $doSwap = $true
    if (Test-Path $rewireExe) {
        $oldTs = (Get-Item $rewireExe).LastWriteTime
        $newTs = (Get-Item $rewireNewExe).LastWriteTime
        if ($newTs -le $oldTs) {
            Write-Host "-> rewire.exe is already newer than rewire-new.exe; not swapping" -ForegroundColor DarkGray
            $doSwap = $false
        }
    }
    if ($doSwap) {
        # The running process may still hold a handle for a moment after
        # Stop-Process. Retry the copy a few times.
        $tries = 0
        while ($tries -lt 10) {
            try {
                Copy-Item $rewireNewExe $rewireExe -Force -ErrorAction Stop
                Write-Host "-> swapped bin\rewire.exe <- bin\rewire-new.exe" -ForegroundColor Green
                break
            } catch {
                $tries++
                Start-Sleep -Milliseconds 500
            }
        }
        if ($tries -ge 10) {
            Write-Host "WARNING: could not swap binary (file lock). Supervisor will relaunch old binary." -ForegroundColor Yellow
        }
    }
}

# Wait for the supervisor to be relaunched by the next /Run, then for it
# to start rewire. Restart the scheduled task FIRST (it spawns a new
# powershell.exe with the fresh supervise.ps1).
Write-Host "-> kicking RewireSupervisor scheduled task ..." -ForegroundColor Cyan
$runResult = schtasks.exe /Run /TN RewireSupervisor 2>&1
Write-Host "  run: $runResult"

# Wait for the supervisor to spawn rewire (it runs immediately on launch,
# not after the first 30-second sleep).
Write-Host "-> waiting for new rewire process (<=90 s) ..." -ForegroundColor Cyan
$deadline = (Get-Date).AddSeconds(90)
$newPid = $null
while ((Get-Date) -lt $deadline) {
    Start-Sleep -Seconds 3
    if (Test-Path $pidFile) {
        $candidate = (Get-Content $pidFile -Raw).Trim()
        if ($candidate -match '^\d+$' -and (-not $oldPid -or $candidate -ne $oldPid)) {
            try {
                Get-Process -Id $candidate -ErrorAction Stop | Out-Null
                $newPid = $candidate
                break
            } catch {}
        }
    }
}
if (-not $newPid) {
    Write-Host "WARNING: supervisor did not relaunch within 90 s. Check the supervisor log." -ForegroundColor Yellow
    Write-Host "  schtasks /Run /TN RewireSupervisor" -ForegroundColor DarkGray
    exit 2
}
Write-Host "-> rewire relaunched, pid=$newPid" -ForegroundColor Green

# Probe /api/version. Windows PowerShell 5.1's HTTP stack often fails on
# rewire's self-signed cert / TLS 1.3 setup, so use curl.exe (always
# present on Windows 10/11) with -k (insecure) for the verification.
Write-Host "-> verifying /api/version ..." -ForegroundColor Cyan
$verDeadline = (Get-Date).AddSeconds(30)
$ver = $null
while ((Get-Date) -lt $verDeadline) {
    $resp = curl.exe -sk --max-time 5 'https://127.0.0.1:9999/api/version' 2>$null
    if ($LASTEXITCODE -eq 0 -and $resp) {
        try {
            $ver = $resp | ConvertFrom-Json -ErrorAction Stop
            if ($ver) { break }
        } catch {}
    }
    Start-Sleep -Seconds 2
}
if (-not $ver) {
    Write-Host "ERROR: /api/version did not respond. Check stderr.log." -ForegroundColor Red
    exit 3
}

Write-Host ""
Write-Host "=== RESULT ===" -ForegroundColor Green
Write-Host "build version:    $($ver.version)"
Write-Host "dodo_enabled:     $($ver.dodo_enabled)"
Write-Host "dodo_env:         $($ver.dodo_env)"
Write-Host "dodo_product_id:  $($ver.dodo_product_id)"
Write-Host "rzp_enabled:      $($ver.rzp_enabled)  (legacy -- should be false now)"

if ($ver.dodo_enabled -ne $true) {
    Write-Host ""
    Write-Host "FAIL: dodo_enabled is not true. Check stderr.log for missing env vars." -ForegroundColor Red
    Write-Host "  Get-Content '$RuntimeDir\stderr.log' -Tail 50" -ForegroundColor DarkGray
    exit 4
}
if ($ver.dodo_product_id -ne $productId) {
    Write-Host ""
    Write-Host "FAIL: server reports a different dodo_product_id ($($ver.dodo_product_id)) than expected ($productId)." -ForegroundColor Red
    exit 5
}

Write-Host ""
Write-Host "PRODUCTION DODO PAYMENTS READY." -ForegroundColor Green
Write-Host "Open https://rewire.gogillu.live/buy and click the new Rs.9 button to test." -ForegroundColor White
