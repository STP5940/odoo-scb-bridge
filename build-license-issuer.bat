@echo off
cd /d "%~dp0"
if not exist tools mkdir tools

if not exist ".secrets\issuer-ed25519-private.pem" (
    echo [!] Issuer signing key is missing: .secrets\issuer-ed25519-private.pem
    echo     Restore the original key from the encrypted backup on the issuer machine.
    echo     Do not generate a replacement unless you are prepared to rebuild and redistribute the app.
    exit /b 1
)

echo Building internal license issuer tool...
go build -ldflags "-s -w" -o tools\OdooSCBBridgeLicenseIssuer.exe .\cmd\license-issuer
if errorlevel 1 (
    echo [!] License issuer build failed.
    exit /b 1
)
echo [OK] tools\OdooSCBBridgeLicenseIssuer.exe
