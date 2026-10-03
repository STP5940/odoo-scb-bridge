$ErrorActionPreference = 'Stop'
# Keep Ctrl+C from stopping only Read-Host while the parent build.bat continues.
$cancelHandler = [ConsoleCancelEventHandler]{
    param($sender, $eventArgs)
    $eventArgs.Cancel = $true
    [Console]::Error.WriteLine('Build cancelled by Ctrl+C.')
    [Environment]::Exit(130)
}
[Console]::add_CancelKeyPress($cancelHandler)

$root = Split-Path -Parent $PSScriptRoot
$packagePath = Join-Path $root 'package.json'
$currentText = [System.IO.File]::ReadAllText($packagePath)
$versionMatch = [regex]::Match($currentText, '(?m)^\s*"version"\s*:\s*"(\d+\.\d+\.\d+)"')
if (-not $versionMatch.Success) {
    throw 'package.json must contain a three-part semantic version.'
}

$currentVersion = $versionMatch.Groups[1].Value
Write-Host ''
Write-Host "Current Version: $currentVersion"
Write-Host '--------------------------------------------------'
$level = ''
while ($level -notin @('1', '2', '3', 'Q')) {
    Write-Host ''
    Write-Host 'Select Version Bump:'
    Write-Host '  [1] Patch (Bug fixes)'
    Write-Host '  [2] Minor (New features)'
    Write-Host '  [3] Major (Breaking changes)'
    Write-Host '  [Q] Cancel build'
    $level = (Read-Host 'Enter choice [1-3, or Q to cancel] (Default is 1: Patch)').Trim().ToUpperInvariant()
    if (-not $level) {
        $level = '1'
    }
}

if ($level -eq 'Q') {
    Write-Host 'Build cancelled.'
    exit 1
}

$parts = [long[]]($currentVersion.Split('.') | ForEach-Object { [long]::Parse($_) })
switch ($level) {
    '1' { $parts[2]++ }
    '2' { $parts[1]++; $parts[2] = 0 }
    '3' { $parts[0]++; $parts[1] = 0; $parts[2] = 0 }
}
$newVersion = '{0}.{1}.{2}' -f $parts[0], $parts[1], $parts[2]

function Update-OneMatch([string]$relativePath, [string]$pattern, [string]$replacement) {
    $path = Join-Path $root $relativePath
    $content = [System.IO.File]::ReadAllText($path)
    $matches = [regex]::Matches($content, $pattern)
    if ($matches.Count -ne 1) {
        throw "Expected one version entry in $relativePath, found $($matches.Count)."
    }
    $regex = [regex]::new($pattern)
    $updated = $regex.Replace($content, $replacement, 1)
    [System.IO.File]::WriteAllText($path, $updated, [System.Text.UTF8Encoding]::new($false))
}

Update-OneMatch 'package.json' '(?m)(^\s*"version"\s*:\s*")[^"]+("\s*,?)' ('${1}' + $newVersion + '${2}')
Update-OneMatch 'package-lock.json' '(?m)(\A\{\r?\n\s*"name"\s*:\s*"[^"]+",\r?\n\s*"version"\s*:\s*")[^"]+' ('${1}' + $newVersion)
Update-OneMatch 'package-lock.json' '(?m)(^\s*""\s*:\s*\{\r?\n\s*"name"\s*:\s*"[^"]+",\r?\n\s*"version"\s*:\s*")[^"]+' ('${1}' + $newVersion)
Update-OneMatch 'installer.iss' '(?m)^AppVersion=.*$' ('AppVersion=' + $newVersion)
Update-OneMatch 'internal\ui\index.html' 'Studio v\d+\.\d+(?:\.\d+)?' ('Studio v' + $newVersion)
Update-OneMatch 'internal\ui\index.html' 'CORE v\d+\.\d+(?:\.\d+)?' ('CORE v' + $newVersion)
Update-OneMatch 'internal\ui\index.html' '(?<=<span class="font-mono text-\[10px\] text-slate-400">)v\d+\.\d+\.\d+(?=</span>)' ('v' + $newVersion)
Update-OneMatch 'internal\appversion\version.go' '(?m)^const Current = "[^"]+"$' ('const Current = "' + $newVersion + '"')

$levelName = switch ($level) {
    '1' { 'Patch' }
    '2' { 'Minor' }
    '3' { 'Major' }
}
Write-Host ''
Write-Host "Version Mode : PRODUCTION ($levelName release)"
Write-Host "Version Plan : $currentVersion -> $newVersion"
Write-Host 'Target Output: dist\bridge_service.exe; dist_electron\win-unpacked\; output\OdooSCBBridgeSetup.exe'
Write-Host '--------------------------------------------------'
