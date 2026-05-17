# audit-posters.ps1 — v1.6.2
#
# HEADs every poster_url returned by /api/movies, classifies as
# OK / BROKEN. Writes:
#   audit-posters.json      — full per-movie result
#   audit-posters-broken.txt — just the broken ones
#
# Run from anywhere. Uses -SkipCertificateCheck for the localhost
# self-signed cert. Polite: 200ms between requests so we don't burn
# the upstream Wikimedia CDN.

[CmdletBinding()]
param(
    [string]$Api = 'https://localhost:9999/api/movies',
    [string]$OutDir = 'C:\Users\arushi\.copilot\session-state\abd0700f-1772-4351-81ba-707e1cdfc3db\files\rewire-runtime'
)

$ErrorActionPreference = 'Stop'

$resp = Invoke-WebRequest -Uri $Api -SkipCertificateCheck -UseBasicParsing
$json = $resp.Content | ConvertFrom-Json
Write-Host "Auditing $($json.count) movies..." -ForegroundColor Cyan

# Polite UA + 429-aware retry. Wikimedia rate-limits anonymous bursts
# from a single IP. We back off on 429 and retry up to 3 times.
$ua = @{ 'User-Agent' = 'rewire-poster-audit/1.0 (https://gogillu.in; admin@gogillu.in)' }

function Test-Poster([string]$url) {
    $out = @{ status = 'BROKEN'; http = 0; ctype = ''; bytes = 0; error = '' }
    if ([string]::IsNullOrWhiteSpace($url) -or $url -eq 'none') {
        $out.status = 'EMPTY'
        return $out
    }
    for ($attempt = 1; $attempt -le 3; $attempt++) {
        try {
            $h = Invoke-WebRequest -Uri $url -Method Head -UseBasicParsing -TimeoutSec 10 -MaximumRedirection 5 -Headers $ua
            $out.http  = [int]$h.StatusCode
            $out.ctype = "$($h.Headers.'Content-Type')"
            $out.bytes = [int64]([string]::Concat($h.Headers.'Content-Length') -as [int64])
            if ($h.StatusCode -eq 200 -and $out.ctype -match '^image/') {
                $out.status = 'OK'
            } else {
                $out.error = "http=$($h.StatusCode) ctype=$($out.ctype)"
            }
            return $out
        } catch {
            $msg = $_.Exception.Message
            $out.error = $msg
            if ($msg -match '429') {
                # Exponential backoff: 1.5s, 4.5s, 13.5s.
                Start-Sleep -Seconds ([int]([Math]::Pow(3, $attempt) * 0.5))
                continue
            }
            return $out
        }
    }
    return $out
}

$results = @()
$i = 0
foreach ($m in $json.movies) {
    $i++
    $url = $m.poster_url
    $entry = [ordered]@{
        id     = $m.id
        title  = $m.title
        year   = $m.year
        region = $m.region
        kind   = $m.kind
        url    = $url
        status = ''
        http   = 0
        ctype  = ''
        bytes  = 0
        error  = ''
    }
    $r = Test-Poster $url
    $entry.status = $r.status
    $entry.http   = $r.http
    $entry.ctype  = $r.ctype
    $entry.bytes  = $r.bytes
    $entry.error  = $r.error
    Write-Host ("[{0,3}/{1}] {2,-8} {3}" -f $i, $json.count, $entry.status, $m.id)
    $results += [PSCustomObject]$entry
    Start-Sleep -Milliseconds 350
}

$jsonPath   = Join-Path $OutDir 'audit-posters.json'
$brokenPath = Join-Path $OutDir 'audit-posters-broken.txt'
$results | ConvertTo-Json -Depth 4 | Set-Content -Path $jsonPath -Encoding UTF8
$broken = $results | Where-Object { $_.status -ne 'OK' }
$broken | ForEach-Object { "$($_.status) $($_.id) :: $($_.title) ($($_.year)) :: $($_.url) :: $($_.error)" } |
    Set-Content -Path $brokenPath -Encoding UTF8

Write-Host ""
Write-Host "==== Summary ====" -ForegroundColor Yellow
$results | Group-Object status | ForEach-Object { Write-Host "  $($_.Name): $($_.Count)" }
Write-Host "Full report : $jsonPath"
Write-Host "Broken list : $brokenPath"
