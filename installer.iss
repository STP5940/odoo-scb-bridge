#ifndef SourceDir
  #define SourceDir "dist"
#endif
#ifndef OutputDir
  #define OutputDir "output"
#endif

[Setup]
AppName=Odoo SCB Bridge Service
AppVersion=0.1.73
DefaultDirName={autopf}\OdooSCBBridge
DefaultGroupName=Odoo SCB Bridge
OutputDir={#OutputDir}
OutputBaseFilename=OdooSCBBridgeSetup
Compression=lzma2
SolidCompression=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x64
ArchitecturesInstallIn64BitMode=x64
SetupIconFile=icon\app.ico
UninstallDisplayIcon={app}\ServiceMonitor.exe
CloseApplications=no

[Files]
Source: "{#SourceDir}\bridge_service.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "dist_electron\win-unpacked\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[Dirs]
Name: "{app}\data"; Permissions: users-modify
Name: "{app}\data\inbound"; Permissions: users-modify
Name: "{app}\data\temp"; Permissions: users-modify
Name: "{app}\data\outbound"; Permissions: users-modify

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"

[Icons]
Name: "{group}\Odoo SCB Bridge Monitor"; Filename: "{app}\ServiceMonitor.exe"; IconFilename: "{app}\ServiceMonitor.exe"
Name: "{autodesktop}\Odoo SCB Bridge Monitor"; Filename: "{app}\ServiceMonitor.exe"; Tasks: desktopicon; IconFilename: "{app}\ServiceMonitor.exe"

[Run]
Filename: "{app}\bridge_service.exe"; Parameters: "install"; Flags: runhidden waituntilterminated
Filename: "sc.exe"; Parameters: "sdset OdooSCBBridge D:(A;;CCLCSWRPWPDTLOCRRC;;;SY)(A;;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;BA)(A;;CCLCSWRPWPDTLOCRRC;;;IU)(A;;CCLCSWLOCRRC;;;SU)"; Flags: runhidden
Filename: "netsh.exe"; Parameters: "advfirewall firewall add rule name=""Odoo SCB Bridge SFTP"" dir=in action=allow protocol=TCP localport=2222"; Flags: runhidden
Filename: "{app}\bridge_service.exe"; Parameters: "start"; Flags: runhidden
Filename: "{app}\ServiceMonitor.exe"; Description: "Launch Odoo SCB Bridge Monitor"; Flags: nowait postinstall skipifsilent

[UninstallRun]
Filename: "{app}\bridge_service.exe"; Parameters: "stop"; Flags: runhidden
Filename: "{app}\bridge_service.exe"; Parameters: "uninstall"; Flags: runhidden
Filename: "netsh.exe"; Parameters: "advfirewall firewall delete rule name=""Odoo SCB Bridge SFTP"""; Flags: runhidden

[Code]
procedure CurStepChanged(CurStep: TSetupStep);
var
  ResultCode: Integer;
begin
  if CurStep = ssInstall then
  begin
    Exec('cmd.exe', '/c net stop OdooSCBBridge', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
    Exec('taskkill.exe', '/F /IM bridge_service.exe /T', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
    Exec('taskkill.exe', '/F /IM ServiceMonitor.exe /T', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
  end;
  if CurStep = ssPostInstall then
  begin
    SaveStringToFile(ExpandConstant('{app}\data\version-install-time.txt'), GetDateTimeString('yyyy-mm-dd hh:nn:ss', '-', ':') + #13#10, False);
  end;
end;
