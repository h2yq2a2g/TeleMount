; TeleMount Windows Setup (Inno Setup 6)
; Bundles telemount.exe + official WinFsp MSI and silently installs WinFsp if missing.

#ifndef AppVersion
  #define AppVersion "1.0.0"
#endif
#ifndef AppExePath
  #define AppExePath "..\dist\telemount.exe"
#endif
#ifndef WinFspMsiPath
  #define WinFspMsiPath "..\dist\winfsp.msi"
#endif
#ifndef OutputDir
  #define OutputDir "..\dist"
#endif

[Setup]
AppId={{4A7E2B19-6C3D-4F81-B905-2D8A7E1C4F33}
AppName=TeleMount
AppVersion={#AppVersion}
AppPublisher=TeleMount
DefaultDirName={autopf}\TeleMount
DefaultGroupName=TeleMount
DisableProgramGroupPage=yes
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
; Admin required once so bundled WinFsp kernel driver MSI can install silently.
PrivilegesRequired=admin
; Startup task uses HKCU to match internal/desktop/startup_windows.go (no UAC needed to toggle later).
UsedUserAreasWarning=no
OutputDir={#OutputDir}
OutputBaseFilename=TeleMount-Setup
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
CloseApplications=yes
CloseApplicationsFilter=telemount.exe
RestartApplications=no
SetupIconFile=..\assets\icon.ico
UninstallDisplayIcon={app}\telemount.exe

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked
Name: "startup"; Description: "Run TeleMount automatically at Windows startup (minimized to tray)"; GroupDescription: "Startup:"

[Files]
Source: "{#AppExePath}"; DestDir: "{app}"; DestName: "telemount.exe"; Flags: ignoreversion
Source: "{#WinFspMsiPath}"; DestDir: "{tmp}"; DestName: "winfsp.msi"; Flags: deleteafterinstall; Check: NeedsWinFsp

[Icons]
Name: "{autoprograms}\TeleMount"; Filename: "{app}\telemount.exe"
Name: "{autodesktop}\TeleMount"; Filename: "{app}\telemount.exe"; Tasks: desktopicon

[Registry]
; Matches internal/desktop/startup_windows.go ("TeleMount" -> "<exe>" --minimized)
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "TeleMount"; ValueData: """{app}\telemount.exe"" --minimized"; Flags: uninsdeletevalue; Tasks: startup

[Run]
; runasoriginaluser is critical so telemount.exe runs non-elevated (visible V: drive & user %LocalAppData%).
Filename: "{app}\telemount.exe"; Description: "{cm:LaunchProgram,TeleMount}"; Flags: nowait postinstall skipifsilent runasoriginaluser

[Code]
var
  WinFspRebootRequired: Boolean;

function NeedsWinFsp: Boolean;
var
  InstallDir: String;
begin
  // Match internal/desktop/winfsp_windows.go detection (32-bit HKLM\SOFTWARE\WinFsp + winfsp-x64.dll).
  if RegQueryStringValue(HKLM32, 'SOFTWARE\WinFsp', 'InstallDir', InstallDir) then
  begin
    if FileExists(AddBackslash(InstallDir) + 'bin\winfsp-x64.dll') then
    begin
      Result := False;
      Exit;
    end;
  end;
  if FileExists(ExpandConstant('{commonpf32}\WinFsp\bin\winfsp-x64.dll')) then
  begin
    Result := False;
    Exit;
  end;
  Result := True;
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  MsiPath: String;
  ResultCode: Integer;
begin
  if (CurStep = ssPostInstall) and NeedsWinFsp then
  begin
    MsiPath := ExpandConstant('{tmp}\winfsp.msi');
    if not FileExists(MsiPath) then
      Exit;

    WizardForm.StatusLabel.Caption := 'Installing WinFsp Virtual Drive driver...';
    WizardForm.FilenameLabel.Caption := 'winfsp.msi';

    if not Exec(ExpandConstant('{sys}\msiexec.exe'), '/i "' + MsiPath + '" /qn /norestart', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
    begin
      MsgBox('Could not launch msiexec to install WinFsp. TeleMount is installed, but you will need to install WinFsp manually from https://winfsp.dev to mount the virtual drive.', mbError, MB_OK);
      Exit;
    end;

    if (ResultCode = 3010) or (ResultCode = 1641) then
    begin
      WinFspRebootRequired := True;
    end
    else if ResultCode <> 0 then
    begin
      MsgBox('WinFsp installation exited with code ' + IntToStr(ResultCode) + '. TeleMount is installed, but you may need to install WinFsp manually from https://winfsp.dev.', mbError, MB_OK);
    end;
  end;
end;

function NeedRestart: Boolean;
begin
  Result := WinFspRebootRequired;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usUninstall then
  begin
    // Remove startup entry even if it was enabled later via Tray or Web UI.
    RegDeleteValue(HKCU, 'Software\Microsoft\Windows\CurrentVersion\Run', 'TeleMount');
  end;
end;
