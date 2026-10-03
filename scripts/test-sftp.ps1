$ErrorActionPreference = 'Stop'
$cancelHandler = [ConsoleCancelEventHandler]{
    param($sender, $eventArgs)
    $eventArgs.Cancel = $true
    [Console]::Error.WriteLine("`n[CANCELLED] SFTP upload test was cancelled.")
    [Environment]::Exit(130)
}
[Console]::add_CancelKeyPress($cancelHandler)

$hostName = Read-Host 'SFTP host/IP [127.0.0.1]'
if ([string]::IsNullOrWhiteSpace($hostName)) { $hostName = '127.0.0.1' }

$port = 0
while ($port -lt 1 -or $port -gt 65535) {
    $portText = Read-Host 'SFTP port [2222]'
    if ([string]::IsNullOrWhiteSpace($portText)) { $portText = '2222' }
    if (-not [int]::TryParse($portText, [ref]$port) -or $port -lt 1 -or $port -gt 65535) {
        Write-Host '[INPUT] Enter a port number between 1 and 65535.' -ForegroundColor Yellow
        $port = 0
    }
}

$username = Read-Host 'SFTP username'
while ([string]::IsNullOrWhiteSpace($username)) {
    Write-Host '[INPUT] Username is required. Please enter your SFTP account name.' -ForegroundColor Yellow
    $username = Read-Host 'SFTP username'
}

$password = Read-Host 'SFTP password' -AsSecureString
$passwordPointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($password)
$exitCode = 1
try {
    $env:SFTP_PROBE_PASSWORD = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($passwordPointer)
    Push-Location (Join-Path $PSScriptRoot '..')
    try {
        & go run ./scripts/sftp-probe -host $hostName -port $port -user $username
        $exitCode = $LASTEXITCODE
    }
    finally {
        Pop-Location
    }
}
finally {
    Remove-Item Env:SFTP_PROBE_PASSWORD -ErrorAction SilentlyContinue
    [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($passwordPointer)
}

exit $exitCode
