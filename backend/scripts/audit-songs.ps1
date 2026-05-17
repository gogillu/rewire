# audit-songs.ps1 — v1.6.2
#
# For every movie returned by /api/movies (the 97 user-visible ones)
# checks the on-disk audio at data/audio/<id>.mp3:
#
#   OK        : exists, > 50KB, valid MP3 header (ID3 / 0xFFFB / 0xFFFA / 0xFFF3)
#   TINY      : file exists but < 50KB (matches existing Server logic)
#   MISSING   : file does not exist
#   CORRUPT   : > 50KB but no valid MP3 header (likely HTML error body)
#
# Writes:
#   audit-songs.json            — full per-movie result
#   audit-songs-broken.txt      — non-OK entries in a flat format
#   audit-songs-manual-review.md — hand-curatable list with song_query
#                                  hints so the user can re-run the
#                                  yt-dlp pipeline on just these IDs.
#
# We do NOT attempt to re-download here. yt-dlp/ffmpeg pipeline lives
# at backend/song_pipeline.py and is the right place to bulk-refresh.

[CmdletBinding()]
param(
    [string]$Api      = 'https://localhost:9999/api/movies',
    [string]$AudioDir = 'C:\Users\arushi\Rewire\data\audio',
    [string]$OutDir   = 'C:\Users\arushi\.copilot\session-state\abd0700f-1772-4351-81ba-707e1cdfc3db\files\rewire-runtime',
    [string]$QueriesPath = 'C:\Users\arushi\.copilot\session-state\abd0700f-1772-4351-81ba-707e1cdfc3db\files\rewire-runtime\audio-queries.json'
)
$ErrorActionPreference = 'Stop'

$resp = Invoke-WebRequest -Uri $Api -SkipCertificateCheck -UseBasicParsing
$json = $resp.Content | ConvertFrom-Json
Write-Host "Auditing $($json.count) songs..." -ForegroundColor Cyan

$queries = @{}
if (Test-Path $QueriesPath) {
    try { $queries = (Get-Content $QueriesPath -Raw | ConvertFrom-Json) } catch { $queries = @{} }
}

function Test-Mp3Header($path) {
    try {
        $fs = [System.IO.File]::OpenRead($path)
        $buf = New-Object byte[] 4
        [void]$fs.Read($buf, 0, 4)
        $fs.Close()
        # ID3v2 tag.
        if ($buf[0] -eq 0x49 -and $buf[1] -eq 0x44 -and $buf[2] -eq 0x33) { return $true }
        # MPEG audio frame sync 11-bit: 0xFFE? (FFFB / FFFA / FFF3 / FFF2 are common).
        if ($buf[0] -eq 0xFF -and ($buf[1] -band 0xE0) -eq 0xE0) { return $true }
        return $false
    } catch { return $false }
}

$results = @()
$i = 0
foreach ($m in $json.movies) {
    $i++
    $path = Join-Path $AudioDir ($m.id + '.mp3')
    $entry = [ordered]@{
        id      = $m.id
        title   = $m.title
        year    = $m.year
        region  = $m.region
        kind    = $m.kind
        path    = $path
        bytes   = 0
        status  = ''
        query   = $null
    }
    if ($queries.PSObject.Properties.Name -contains $m.id) {
        $entry.query = $queries.($m.id)
    }
    if (-not (Test-Path $path)) {
        $entry.status = 'MISSING'
    } else {
        $info = Get-Item $path
        $entry.bytes = $info.Length
        if ($info.Length -lt 50000) {
            $entry.status = 'TINY'
        } elseif (-not (Test-Mp3Header $path)) {
            $entry.status = 'CORRUPT'
        } else {
            $entry.status = 'OK'
        }
    }
    Write-Host ("[{0,3}/{1}] {2,-8} {3}" -f $i, $json.count, $entry.status, $m.id)
    $results += [PSCustomObject]$entry
}

$jsonPath   = Join-Path $OutDir 'audit-songs.json'
$brokenPath = Join-Path $OutDir 'audit-songs-broken.txt'
$mdPath     = Join-Path $OutDir 'audit-songs-manual-review.md'
$results | ConvertTo-Json -Depth 4 | Set-Content -Path $jsonPath -Encoding UTF8
$broken = $results | Where-Object { $_.status -ne 'OK' }
$broken | ForEach-Object { "$($_.status) $($_.id) :: $($_.title) ($($_.year)) :: $($_.bytes)B :: query=$($_.query)" } |
    Set-Content -Path $brokenPath -Encoding UTF8

# Markdown report so the user can flip through and pick replacements.
$md = @()
$md += "# Songs that need manual review"
$md += ""
$md += "Generated $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') by audit-songs.ps1."
$md += ""
$md += "Re-run the yt-dlp pipeline on just these IDs:"
$md += ""
$md += '```powershell'
$md += '# from c:\Users\arushi\Rewire\backend'
$md += "python song_pipeline.py --ids " + (($broken | ForEach-Object { $_.id }) -join ',')
$md += '```'
$md += ""
$md += "| Status | ID | Title | Year | Bytes | song_query (current) |"
$md += "|--------|----|-------|------|-------|----------------------|"
foreach ($b in $broken) {
    $q = if ($b.query) { ("``" + $b.query + "``") } else { "_(none — please add to audio-queries.json)_" }
    $md += "| $($b.status) | $($b.id) | $($b.title) | $($b.year) | $($b.bytes) | $q |"
}
$md | Set-Content -Path $mdPath -Encoding UTF8

Write-Host ""
Write-Host "==== Summary ====" -ForegroundColor Yellow
$results | Group-Object status | ForEach-Object { Write-Host "  $($_.Name): $($_.Count)" }
Write-Host "Full report      : $jsonPath"
Write-Host "Broken list      : $brokenPath"
Write-Host "Manual-review MD : $mdPath"
