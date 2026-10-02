@echo off
setlocal
cd /d "%~dp0"
chcp 65001 >nul

echo ========================================================
echo       Odoo SCB Bridge SFTP Upload Test
echo ========================================================
echo The probe is uploaded to inbound; scheduled jobs may move it to archive.
echo.

where go >nul 2>nul
if errorlevel 1 (
    echo [!] Go is required to run this test. Install Go and try again.
    exit /b 1
)

powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\test-sftp.ps1"
set "RESULT=%ERRORLEVEL%"

echo.
if "%RESULT%"=="130" goto cancelled
if "%RESULT%"=="0" goto passed
echo [FAIL] SFTP upload test failed. Review the error above.
exit /b %RESULT%

:cancelled
echo [CANCELLED] SFTP upload test was cancelled.
exit /b 130

:passed
echo [PASS] SFTP upload test completed successfully.
exit /b 0
