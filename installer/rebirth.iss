; The Windows installer for Rebirth, for Inno Setup 6.3 or later.
;
; Built by scripts\build-installer.ps1, which passes the version from
; app\pubspec.yaml and has just built dist\rebirth-windows:
;
;   ISCC /DVersion=1.2.0 /DBuild=12 installer\rebirth.iss
;
; What it does, besides copying files:
;   - asks a running copy to leave the proper way first (Rebirth.exe --quit), so
;     its tunnel comes down and the machine gets its names back, instead of the
;     program being killed with both left in place;
;   - offers "start with Windows": a task that starts the program when someone
;     signs in, hidden in the notification area, connecting by itself;
;   - starts the program again when it was run silently, which is how the
;     program updates itself: it runs this setup with /SILENT and leaves.
;   - takes the program's own tunnel with it (the tunnel folder: awgtunnel.exe and
;     wintun.dll, made by scripts\build-tunnel.ps1 and put in dist\rebirth-windows
;     by scripts\build-windows.ps1), so nothing else has to be installed. When the
;     program is removed, the tunnel services it made go with it.
;
; The file is UTF-8 with a byte order mark, which Inno Setup needs to read the
; Russian text.

#ifndef Version
  #error Pass /DVersion=1.2.3 and /DBuild=4, or build with scripts\build-installer.ps1
#endif
#ifndef Build
  #define Build "0"
#endif

#define AppName "Rebirth"
#define AppExe "Rebirth.exe"
#define AutostartTask "Rebirth"
; The names of the tunnels the program makes (lib/core: awgr-a and awgr-b take turns,
; awgr-enroll registers a new device).
#define TunnelExe "{app}\tunnel\awgtunnel.exe"

[Setup]
; Never change: it is what makes a new setup an update of the installed program.
AppId={{A6F2860A-9921-4FC2-9343-6C81685A9E66}
AppName={#AppName}
AppVersion={#Version}
VersionInfoVersion={#Version}.{#Build}
AppPublisher=Rebirth
DefaultDirName={autopf}\{#AppName}
DefaultGroupName={#AppName}
DisableProgramGroupPage=yes
; The program needs administrator rights itself (tunnels, name rules), so the
; installer asks once, and what it starts afterwards has them already.
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
SetupIconFile=..\app\windows\runner\resources\app_icon.ico
UninstallDisplayIcon={app}\{#AppExe}
OutputDir=..\dist\installers
OutputBaseFilename=Rebirth-Setup-{#Version}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
; The program's own window hides when closed, so it is asked to leave by hand in
; PrepareToInstall. This catches anything else holding the files.
CloseApplications=yes
RestartApplications=no

[Languages]
Name: "russian"; MessagesFile: "compiler:Languages\Russian.isl"

[CustomMessages]
russian.TaskAutostart=Запускать вместе с Windows (свернутым в значок, с подключением)
russian.TaskDesktopIcon=Значок на рабочем столе
russian.StatusAutostart=Настраиваю запуск вместе с Windows…
russian.RunApp=Запустить Rebirth

[Tasks]
Name: "autostart"; Description: "{cm:TaskAutostart}"
Name: "desktopicon"; Description: "{cm:TaskDesktopIcon}"; Flags: unchecked

[Files]
Source: "..\dist\rebirth-windows\*"; DestDir: "{app}"; Flags: recursesubdirs createallsubdirs ignoreversion
Source: "third-party-notices.txt"; DestDir: "{app}"; Flags: ignoreversion
Source: "autostart.ps1"; DestDir: "{tmp}"; Flags: deleteafterinstall; Tasks: autostart

[Icons]
Name: "{group}\{#AppName}"; Filename: "{app}\{#AppExe}"
Name: "{autodesktop}\{#AppName}"; Filename: "{app}\{#AppExe}"; Tasks: desktopicon

[Run]
Filename: "{sys}\WindowsPowerShell\v1.0\powershell.exe"; Parameters: "-NoProfile -ExecutionPolicy Bypass -File ""{tmp}\autostart.ps1"" -Exe ""{app}\{#AppExe}"""; Flags: runhidden waituntilterminated; StatusMsg: "{cm:StatusAutostart}"; Tasks: autostart
; Not chosen (now, or in the setup this one replaces): the task is taken away.
Filename: "{sys}\schtasks.exe"; Parameters: "/Delete /TN ""{#AutostartTask}"" /F"; Flags: runhidden waituntilterminated; Tasks: not autostart
; After a setup run by hand: the usual "start it now" box. After a silent one (the
; program updating itself): started again without asking.
;
; runascurrentuser matters for the first: an entry with postinstall is started with
; the credentials of the person who ran Setup, not of Setup, which for a program
; that asks for administrator rights ends in "the requested operation requires
; elevation". The program is started with Setup's own (administrator) rights.
Filename: "{app}\{#AppExe}"; Description: "{cm:RunApp}"; Flags: nowait postinstall skipifsilent runascurrentuser
Filename: "{app}\{#AppExe}"; Flags: nowait runascurrentuser; Check: WizardSilent

[UninstallRun]
; A tunnel the program left running (it was killed, say) would otherwise stay as a
; service whose file is gone. Taking away one that is not there is not an error.
Filename: "{#TunnelExe}"; Parameters: "uninstall awgr-a"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveTunnelA"
Filename: "{#TunnelExe}"; Parameters: "uninstall awgr-b"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveTunnelB"
Filename: "{#TunnelExe}"; Parameters: "uninstall awgr-enroll"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveTunnelEnroll"
Filename: "{sys}\schtasks.exe"; Parameters: "/Delete /TN ""{#AutostartTask}"" /F"; Flags: runhidden waituntilterminated; RunOnceId: "DeleteAutostartTask"
; The program makes this one itself, to give the machine its names back after a
; restart; with the program gone it has nothing to do.
Filename: "{sys}\schtasks.exe"; Parameters: "/Delete /TN ""awg-rotator cleanup"" /F"; Flags: runhidden waituntilterminated; RunOnceId: "DeleteCleanupTask"

[UninstallDelete]
; The tunnel files the program keeps for the machine. Per-user files (the saved
; state, the log) stay: a reinstall then keeps the device registered.
Type: filesandordirs; Name: "{commonappdata}\awg-rotator"

[Code]
{ Asks the copy that is running to leave by the same way as its menu's "Exit"
  and waits until it has: its tunnel comes down and the machine gets its names
  back. Nothing happens when none is running. If it does not answer in half a
  minute the setup carries on, and the restart manager closes it by force; the
  program cleans up after such an end the next time it starts. }
procedure QuitRunningCopy;
var
  Exe: String;
  Code: Integer;
begin
  Exe := ExpandConstant('{app}\{#AppExe}');
  if FileExists(Exe) then
    Exec(Exe, '--quit', '', SW_HIDE, ewWaitUntilTerminated, Code);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  QuitRunningCopy;
  Result := '';
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usUninstall then
    QuitRunningCopy;
end;
