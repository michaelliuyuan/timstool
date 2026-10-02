# pg-gate.ps1 - one-shot PG regression gate (MS-02).
# Usage:
#   powershell -File scripts\pg-gate.ps1                        # everything
#   powershell -File scripts\pg-gate.ps1 -Face compare          # one face
#   powershell -File scripts\pg-gate.ps1 -Face static,incremental
#   powershell -File scripts\pg-gate.ps1 -ConfigFile my.json    # connection overrides
# Faces: static | wizard | compare | watermark | incremental | cdc | all
# Exit code 0 = all green; non-zero = red light (STOP - do not proceed).
# See docs/PG-GATE.md for prerequisites and the per-commit rule.

param(
    [string]$Face = 'all',
    [string]$ConfigFile = '',
    [switch]$KeepInstance,
    [switch]$SkipStatic
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
. (Join-Path $repoRoot 'scripts\pg-gate-lib.ps1')

$faces = if ($Face -eq 'all') { @('static', 'wizard', 'compare', 'watermark', 'incremental', 'cdc') }
         else { $Face -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ } }
$valid = @('static', 'wizard', 'compare', 'watermark', 'incremental', 'cdc')
foreach ($f in $faces) {
    if ($valid -notcontains $f) { throw "unknown face '$f' (valid: $($valid -join ', ') or all)" }
}
if ($SkipStatic) { $faces = @($faces | Where-Object { $_ -ne 'static' }) }
$needBlackBox = @($faces | Where-Object { $_ -ne 'static' }).Count -gt 0

$cfg = Get-GateConfig -ConfigFile $ConfigFile
Write-Host "pg-gate: faces=[$($faces -join ', ')] repo=$repoRoot"

$workDir = "$($Cfg.gate.workdir)"
if ([string]::IsNullOrWhiteSpace($workDir)) {
    $workDir = Join-Path ([IO.Path]::GetTempPath()) 'pg-gate-run'
}
if (Test-Path -LiteralPath $workDir) { Remove-Item -Recurse -Force -LiteralPath $workDir }
New-Item -ItemType Directory -Force -Path $workDir | Out-Null

$results = @()
$script:GateInst = $null
try {
    if ($needBlackBox) {
        Write-Host "pg-gate: booting scratch instance (port $($cfg.gate.port), workdir $workDir)"
        $script:GateInst = Start-GateInstance -Cfg $cfg -RepoRoot $repoRoot -WorkDir $workDir
        Assert-SourceMatrix
        Write-Host "pg-gate: building PG fixture (schema $($cfg.pg.schema))"
        New-PGFixture $cfg
        $refs = New-GateDataSources $cfg
        # wizard face IS the baseline migration; other faces run one themselves
        $baselineTables = @('orders', 'orders_bulk', 'static_kv')
        if ($faces -contains 'wizard') {
            Invoke-FaceWizard $cfg
        } else {
            $bt = Invoke-BaselineMigration -Cfg $cfg -Tables $baselineTables
            if ($bt.status -ne 'completed') { throw "baseline migration failed: $($bt.error)" }
        }
        if ($faces -contains 'compare')    { Invoke-FaceCompare $cfg }
        if ($faces -contains 'watermark')  { Invoke-FaceWatermark $cfg $refs }
        if ($faces -contains 'incremental') {
            Invoke-FaceIncremental $cfg $refs $repoRoot $workDir $script:GateInst
        }
        if ($faces -contains 'cdc')        { Invoke-FaceCdc $cfg $refs $script:GateInst }
    }
    if ($faces -contains 'static') { Invoke-FaceStatic $cfg $repoRoot }
    foreach ($f in $faces) { $results += @{ face = $f; ok = $true } }
} catch {
    Write-Host "pg-gate: RED - $($_.Exception.Message)" -ForegroundColor Red
    $results += @{ face = 'abort'; ok = $false; error = "$($_.Exception.Message)" }
} finally {
    if (-not $KeepInstance) { Stop-GateInstance $script:GateInst }
}

Write-Host ''
Write-Host '================ pg-gate summary ================'
foreach ($r in $results) {
    if ($r.ok) { Write-Host "  [PASS] $($r.face)" -ForegroundColor Green }
    else { Write-Host "  [FAIL] $($r.face): $($r.error)" -ForegroundColor Red }
}
$failed = @($results | Where-Object { -not $_.ok }).Count
if ($failed -gt 0 -or $results.Count -eq 0) { exit 1 }
Write-Host 'pg-gate: ALL GREEN' -ForegroundColor Green
exit 0
