$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$packagePath = Join-Path $root 'package.json'
$currentText = [System.IO.File]::ReadAllText($packagePath)
$versionMatch = [regex]::Match($currentText, '(?m)^\s*"version"\s*:\s*"(\d+\.\d+\.\d+)"')
if (-not $versionMatch.Success) {
    throw 'package.json must contain a three-part semantic version.'
}

$currentVersion = $versionMatch.Groups[1].Value
$answer = ''
while ($answer -notmatch '^(Y|N)$') {
    $answer = (Read-Host "อัปเดต Version ก่อน Build หรือไม่? [Y/N] (ปัจจุบัน $currentVersion)").Trim().ToUpperInvariant()
}

if ($answer -eq 'N') {
    Write-Host "ใช้ Version เดิม: $currentVersion"
    exit 0
}

$level = ''
while ($level -notin @('1', '2', '3', 'Q')) {
    Write-Host '[1] Major  [2] Minor  [3] Patch  [Q] ยกเลิก Build'
    $level = (Read-Host 'เลือกระดับ Version').Trim().ToUpperInvariant()
}

if ($level -eq 'Q') {
    Write-Host 'ยกเลิก Build ตามคำสั่ง'
    exit 1
}

$parts = [long[]]($currentVersion.Split('.') | ForEach-Object { [long]::Parse($_) })
switch ($level) {
    '1' { $parts[0]++; $parts[1] = 0; $parts[2] = 0 }
    '2' { $parts[1]++; $parts[2] = 0 }
    '3' { $parts[2]++ }
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
Update-OneMatch 'internal\api\api.go' '(?m)("version"\s*:\s*")[^"]+("\s*,?)' ('${1}' + $newVersion + '${2}')

Write-Host "อัปเดต Version: $currentVersion -> $newVersion"
