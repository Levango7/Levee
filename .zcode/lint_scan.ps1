Set-Location 'F:\Nexus\Workflow\levee'
$bom = [byte[]]@(0xEF, 0xBB, 0xBF)
$affected = @()
Get-ChildItem cmd\levee\*.go | Where-Object { $_.Name -notmatch '_test\.go$' } | ForEach-Object {
    $bytes = [System.IO.File]::ReadAllBytes($_.FullName)
    if ($bytes[0] -eq $bom[0] -and $bytes[1] -eq $bom[1] -and $bytes[2] -eq $bom[2]) {
        $noBom = $bytes[3..($bytes.Length-1)]
        [System.IO.File]::WriteAllBytes($_.FullName, $noBom)
        $affected += $_.Name
    }
}
Write-Host "stripped BOM from $($affected.Count) files: $($affected -join ', ')"
