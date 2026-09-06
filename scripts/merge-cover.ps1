#Requires -Version 5.1
<#
.SYNOPSIS
    Merge Go coverage profiles by taking max(count) per block.

.DESCRIPTION
    SQLite and PostgreSQL store tests produce one -coverprofile each; the
    same code block is then covered by whichever backend exercised it. This
    tool merges N profiles into one "combined coverage" profile: blocks are
    keyed by (mode-invariant) file + range + statement count, and a block's
    hit count in the output is the maximum across inputs. Blocks present in
    only one input pass through unchanged.

    Quality-plan tie-in: docs/quality-hardening-2026-09.md E-1 — the
    internal/state "sqlite+PG combined >= 75%" gate is only reproducible
    through this script.

.PARAMETER ProfilePaths
    One or more profile files produced by `go test -coverprofile=...`.

.PARAMETER OutFile
    Destination merged profile (overwritten).

.PARAMETER Filter
    Optional package-path prefix; only blocks from packages starting with
    this prefix are kept (e.g. github.com/nexus/levee/internal/state).

.EXAMPLE
    # Full E-1 sequence (docker postgres per docs/deployment.md):
    go test ./internal/state/... -coverprofile=cover_sqlite.out
    $env:LEVEE_PG_TEST_DSN = 'postgres://levee:levee-ci@localhost:5432/levee_test?sslmode=disable'
    go test ./internal/state/... ./internal/cluster/... -coverprofile=cover_pg.out
    powershell -NoProfile -File scripts\merge-cover.ps1 `
        -ProfilePaths cover_sqlite.out,cover_pg.out -OutFile cover_merged.out `
        -Filter github.com/nexus/levee/internal/state
    go tool cover -func=cover_merged.out | Select-Object -Last 1
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [string[]]$ProfilePaths,
    [Parameter(Mandatory = $true, Position = 1)]
    [string]$OutFile,
    [string]$Filter = ''
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

# key ("file:start.col,end.col stmts") -> max count; ordered output keeps
# first-seen order for stable diffs.
$merged = [ordered]@{}
$seenHeader = $false

foreach ($path in $ProfilePaths) {
    if (-not (Test-Path -LiteralPath $path)) {
        throw "coverage profile not found: $path"
    }
    foreach ($line in (Get-Content -LiteralPath $path)) {
        $line = $line.Trim()
        if ($line -eq '') { continue }
        if ($line.StartsWith('mode:')) { $seenHeader = $true; continue }

        # Block line: "<key> <numStmts> <count>" — count is the last field.
        $fields = $line -split '\s+'
        if ($fields.Count -lt 3) {
            throw "malformed coverage line in ${path}: $line"
        }
        $count = [int]$fields[$fields.Count - 1]
        $key = ($fields[0..($fields.Count - 2)] -join ' ')

        if ($Filter -ne '' -and -not $key.StartsWith($Filter)) { continue }

        if ($merged.Contains($key)) {
            if ($count -gt $merged[$key]) { $merged[$key] = $count }
        }
        else {
            $merged[$key] = $count
        }
    }
}

if (-not $seenHeader) {
    throw "no 'mode:' header found in any input profile"
}

$out = New-Object 'System.Collections.Generic.List[string]'
$out.Add('mode: set')
foreach ($key in $merged.Keys) {
    $out.Add(('{0} {1}' -f $key, $merged[$key]))
}
# BOM-less UTF8: `go tool cover` rejects profiles that start with a BOM, and
# Windows PowerShell's Set-Content -Encoding UTF8 would add one.
[System.IO.File]::WriteAllLines([System.IO.Path]::GetFullPath($OutFile), $out, (New-Object System.Text.UTF8Encoding($false)))
Write-Host ("merged {0} blocks from {1} profile(s) -> {2}" -f $merged.Count, $ProfilePaths.Count, $OutFile)
