# pg-tunnel.ps1 - singleton plink port-forward manager (R1, 2026-10-06,
# track C race diagnosis seq834: uncoordinated tunnel restarts by multiple
# actors caused gate first-reds; see docs/PG-GATE.md "tunnel discipline").
# Usage:
#   powershell -File scripts\pg-tunnel.ps1 -Action start   [-Force]
#   powershell -File scripts\pg-tunnel.ps1 -Action status
#   powershell -File scripts\pg-tunnel.ps1 -Action stop
#   powershell -File scripts\pg-tunnel.ps1 -Action watch   [-CheckSec 15]
# Lock file: %TEMP%\pg-tunnel-<LocalPort>.lock (owner pid). start refuses
# while a live owner holds the lock (-Force kills it first). watch probes
# the forwarded port with a PG SSLRequest probe and restarts plink only
# through the same lock - two watchers can never race.
# Exit code 0 = ok / tunnel healthy; non-zero = error or unhealthy.

param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('start', 'status', 'stop', 'watch')]
    [string]$Action,
    [string]$LocalHost = '127.0.0.1',
    [int]$LocalPort = 15433,
    [string]$RemoteTarget = '127.0.0.1:15433',
    [string]$SshHost = 'root@120.92.109.103',
    [string]$PlinkPath = 'plink',
    [string]$PlinkArgs = '',
    [int]$CheckSec = 15,
    [int]$ProbeTimeoutMs = 3000,
    [switch]$Force
)

$ErrorActionPreference = 'Stop'
$lockFile = Join-Path ([IO.Path]::GetTempPath()) "pg-tunnel-$LocalPort.lock"

function Get-LockOwner {
    if (-not (Test-Path -LiteralPath $lockFile)) { return $null }
    $raw = (Get-Content -LiteralPath $lockFile -ErrorAction SilentlyContinue | Select-Object -First 1)
    $pid2 = 0
    if ([int]::TryParse("$raw".Trim(), [ref]$pid2)) { return $pid2 }
    return $null
}

function Test-PidAlive {
    param([int]$ProcId)
    if ($ProcId -le 0) { return $false }
    $p = Get-Process -Id $ProcId -ErrorAction SilentlyContinue
    return ($null -ne $p -and -not $p.HasExited)
}

function Test-TunnelHealthy {
    $client = New-Object Net.Sockets.TcpClient
    try {
        $iar = $client.BeginConnect($LocalHost, $LocalPort, $null, $null)
        if (-not $iar.AsyncWaitHandle.WaitOne($ProbeTimeoutMs)) { return $false }
        $client.EndConnect($iar)
        $stream = $client.GetStream()
        $stream.WriteTimeout = $ProbeTimeoutMs
        $stream.ReadTimeout = $ProbeTimeoutMs
        $probe = [byte[]](0,0,0,8,4,0xD2,0x16,0x2F)
        $stream.Write($probe, 0, 8)
        $stream.Flush()
        $b = $stream.ReadByte()
        return ($b -eq 83 -or $b -eq 78)
    } catch {
        return $false
    } finally {
        $client.Close()
    }
}

function Start-Tunnel {
    $owner = Get-LockOwner
    if ($null -ne $owner -and (Test-PidAlive -ProcId $owner)) {
        if (-not $Force) {
            Write-Host "pg-tunnel: lock held by live pid $owner - refusing duplicate forward (use -Force to replace)"
            return (Test-TunnelHealthy)
        }
        try { Stop-Process -Id $owner -Force -ErrorAction SilentlyContinue } catch { }
        Start-Sleep -Milliseconds 600
    }
    $argLine = ''
    if ($PlinkArgs.Trim() -ne '') { $argLine = "$($PlinkArgs.Trim()) " }
    $argLine += "-batch -N -L ${LocalHost}:${LocalPort}:${RemoteTarget} $SshHost"
    $proc = Start-Process -FilePath $PlinkPath -ArgumentList $argLine -WindowStyle Hidden -PassThru
    Start-Sleep -Milliseconds 800
    if ($proc.HasExited) {
        Remove-Item -LiteralPath $lockFile -Force -ErrorAction SilentlyContinue
        throw "pg-tunnel: plink exited immediately (code $($proc.ExitCode)) - check host/key"
    }
    Set-Content -LiteralPath $lockFile -Value "$($proc.Id)"
    $deadline = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $deadline) {
        if (Test-TunnelHealthy) {
            Write-Host "pg-tunnel: pid $($proc.Id) forwarding ${LocalHost}:${LocalPort} -> $RemoteTarget (probe green)"
            return $true
        }
        if ($proc.HasExited) { break }
        Start-Sleep -Milliseconds 500
    }
    Write-Warning "pg-tunnel: pid $($proc.Id) started but the forward did not answer the probe in 10s (bind-before-forward window) - probe again with 'status'"
    return $false
}

switch ($Action) {
    'start' {
        $ok = Start-Tunnel
        if (-not $ok) { exit 1 }
        exit 0
    }
    'status' {
        $owner = Get-LockOwner
        $alive = ($null -ne $owner) -and (Test-PidAlive -ProcId $owner)
        $healthy = Test-TunnelHealthy
        Write-Host ("pg-tunnel: lock owner pid={0} alive={1} probe={2} ({3}:{4} -> {5})" -f $owner, $alive, $(if ($healthy) { 'GREEN' } else { 'RED' }), $LocalHost, $LocalPort, $RemoteTarget)
        if ($healthy) { exit 0 } else { exit 1 }
    }
    'stop' {
        $owner = Get-LockOwner
        if ($null -ne $owner) {
            if (Test-PidAlive -ProcId $owner) {
                try { Stop-Process -Id $owner -Force -ErrorAction SilentlyContinue } catch { }
                Write-Host "pg-tunnel: killed owner pid $owner"
            }
            Remove-Item -LiteralPath $lockFile -Force -ErrorAction SilentlyContinue
        } else {
            Write-Host "pg-tunnel: no lock owner"
        }
        exit 0
    }
    'watch' {
        Write-Host "pg-tunnel: watch mode (check every ${CheckSec}s, lock-guarded)"
        while ($true) {
            $owner = Get-LockOwner
            $alive = ($null -ne $owner) -and (Test-PidAlive -ProcId $owner)
            $healthy = $alive -and (Test-TunnelHealthy)
            if (-not $healthy) {
                $stamp = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
                if ($alive) {
                    try { Stop-Process -Id $owner -Force -ErrorAction SilentlyContinue } catch { }
                    Remove-Item -LiteralPath $lockFile -Force -ErrorAction SilentlyContinue
                    Write-Host "[$stamp] pg-tunnel: owner $owner alive but probe RED - killed, restarting"
                } else {
                    Remove-Item -LiteralPath $lockFile -Force -ErrorAction SilentlyContinue
                    Write-Host "[$stamp] pg-tunnel: owner dead - restarting"
                }
                $null = Start-Tunnel
            }
            Start-Sleep -Seconds $CheckSec
        }
    }
}
