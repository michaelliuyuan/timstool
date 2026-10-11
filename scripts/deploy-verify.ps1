# deploy-verify.ps1 - post-deploy acceptance probe (standing, read-only).
# Usage:
#   powershell -File scripts\deploy-verify.ps1 -Password <plink-pw>
#   powershell -File scripts\deploy-verify.ps1 -Password <plink-pw> -Face bundle
#   powershell -File scripts\deploy-verify.ps1 -Password <plink-pw> -ExpectedAsset assets/index-B0fmp9lr.js
# Faces: health | api | bundle | all   (process/pid is always checked)
# bundle face is a DOUBLE proof: the asset referenced by the served index.html
# must equal the asset embedded in the binary AND the one built from
# cmd/static/index.html in this working copy - catches web/dist -> cmd/static
# sync gaps (the M6/M7 embed-staleness class).
# Exit code 0 = all green; non-zero = RED (deployment not accepted).

param(
    [Parameter(Mandatory = $true)]
    [string]$Password,
    [string]$Target = 'root@120.92.109.103',
    [string]$DeployDir = '/opt/timstool-conn',
    [int]$Port = 8080,
    [string]$Face = 'all',
    [string]$ExpectedAsset = ''
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)

$faces = if ($Face -eq 'all') { @('health', 'api', 'bundle') }
         else { $Face -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ } }
$valid = @('health', 'api', 'bundle')
foreach ($f in $faces) {
    if ($valid -notcontains $f) { throw "unknown face '$f' (valid: $($valid -join ', ') or all)" }
}

if ([string]::IsNullOrWhiteSpace($ExpectedAsset)) {
    $htmlPath = Join-Path $repoRoot 'cmd\static\index.html'
    if (-not (Test-Path -LiteralPath $htmlPath)) { throw "cannot derive expected asset: $htmlPath missing (build+sync web/dist -> cmd/static first)" }
    $html = Get-Content -LiteralPath $htmlPath -Raw
    if ($html -match 'assets/(index-[A-Za-z0-9_-]+[.]js)') {
        $ExpectedAsset = 'assets/' + $Matches[1]
    } else {
        throw 'cannot derive expected asset from cmd/static/index.html'
    }
}

$remote = 'cd __DEPLOYDIR__ && ' +
    'echo PID=$(ps -C timstool -o pid=) && ' +
    'echo BINSHA=$(sha256sum timstool | cut -c1-16) && ' +
    'echo HEALTH=$(curl -s -m 5 http://127.0.0.1:__PORT__/api/v1/health) && ' +
    'echo APICODE=$(curl -s -o /dev/null -m 5 -w "%{http_code}" http://127.0.0.1:__PORT__/api/v1/datasources) && ' +
    'echo SERVED=$(curl -s -m 5 http://127.0.0.1:__PORT__/ | grep -o "assets/index-[A-Za-z0-9_-]*[.]js" | head -1) && ' +
    'echo EMBEDDED=$(strings timstool | grep -o "assets/index-[A-Za-z0-9_-]*[.]js" | head -1)'
$remote = $remote.Replace('__DEPLOYDIR__', $DeployDir).Replace('__PORT__', "$Port")

$raw = plink -batch -pw $Password $Target $remote
$map = @{}
foreach ($line in $raw) {
    if ($line -match '^(PID|BINSHA|HEALTH|APICODE|SERVED|EMBEDDED)[=:](.*)$') { $map[$Matches[1]] = $Matches[2].Trim() }
}

$pid_ = "$($map['PID'])"
$binsha = "$($map['BINSHA'])"
Write-Host "deploy-verify: target=$Target dir=$DeployDir faces=[$($faces -join ', ')]"
Write-Host "  pid    = $pid_"
Write-Host "  binsha = $binsha  expected-asset = $ExpectedAsset"

$red = @()

if ([string]::IsNullOrWhiteSpace($pid_)) { $red += 'process: no timstool process found' }

if ($faces -contains 'health') {
    $h = "$($map['HEALTH'])"
    if ($h -match '"status":"ok"') { Write-Host "  [PASS] health   $($h)" }
    else { $red += "health: bad payload: $h" }
}

if ($faces -contains 'api') {
    $c = "$($map['APICODE'])"
    if ($c -eq '200') { Write-Host "  [PASS] api      GET /api/v1/datasources -> 200" }
    else { $red += "api: GET /api/v1/datasources -> '$c' (expected 200)" }
}

if ($faces -contains 'bundle') {
    $served = "$($map['SERVED'])"
    $embedded = "$($map['EMBEDDED'])"
    if ($served -eq $ExpectedAsset) { Write-Host "  [PASS] bundle   served    = $served" }
    else { $red += "bundle: served '$served' != expected '$ExpectedAsset' (stale or missing UI on the wire)" }
    if ($embedded -eq $ExpectedAsset) { Write-Host "  [PASS] bundle   embedded  = $embedded" }
    else { $red += "bundle: embedded '$embedded' != expected '$ExpectedAsset' (binary built from stale cmd/static)" }
}

if ($red.Count -gt 0) {
    foreach ($r in $red) { Write-Host "  [RED ] $r" }
    Write-Host 'deploy-verify: RED - deployment NOT accepted'
    exit 1
}
Write-Host 'deploy-verify: all faces green'
exit 0
