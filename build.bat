@echo off

echo ========================================================
echo       Odoo SCB Bridge Build Script
echo ========================================================

if not exist dist mkdir dist
if not exist output mkdir output

echo.
echo [1/3] Building Microservice (dist\bridge_service.exe)...
go build -ldflags "-s -w" -o dist\bridge_service.exe .\cmd\service
if errorlevel 1 (
    echo [!] Error building bridge_service.exe
    exit /b 1
)

echo.
echo [2/3] Building Electron Desktop App (ServiceMonitor.exe)...
call npm run dist
if errorlevel 1 (
    echo [!] Error packaging Electron Desktop App
    exit /b 1
)
copy /Y dist_electron\win-unpacked\ServiceMonitor.exe dist\ServiceMonitor.exe >nul

echo.
echo [3/3] Compiling Inno Setup GUI Installer (output\OdooSCBBridgeSetup.exe)...

set ISCC_USER=%LOCALAPPDATA%\Programs\Inno Setup 6\ISCC.exe
set ISCC_SYS=C:\Program Files (x86)\Inno Setup 6\ISCC.exe

if exist "%ISCC_USER%" goto run_user
if exist "%ISCC_SYS%" goto run_sys

echo [!] Warning: Inno Setup ISCC.exe not found.
goto end

:run_user
"%ISCC_USER%" /DSourceDir=dist /O"output" installer.iss
if errorlevel 1 exit /b 1
goto end

:run_sys
"%ISCC_SYS%" /DSourceDir=dist /O"output" installer.iss
if errorlevel 1 exit /b 1
goto end

:end
echo.
echo ========================================================
echo   BUILD COMPLETE!
echo   - Binaries (Files): dist\
echo   - Setup Installer : output\OdooSCBBridgeSetup.exe
echo ========================================================
