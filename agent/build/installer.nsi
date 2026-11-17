; PWF Agent per-user installer.
;
; Layout (required by internal/updater):
;   %LOCALAPPDATA%\Programs\PWF Agent\
;     launcher.exe            reads current.txt and starts the target version
;     current.txt             "<version>\r\n"
;     <version>\PWF Agent.exe
;     <version>\tools\        exiftool / ffmpeg (resolved next to the exe)
;     licenses\               third-party notices
;     uninstall.exe
;
; Shortcuts point at launcher.exe, so installing an update only rewrites
; current.txt; a rollback is the same operation in reverse.
;
; Build inputs (makensis -D...):
;   APP_VERSION   release version, e.g. 0.5.0 (required)
;   SOURCE_DIR    staged files, must contain "PWF Agent.exe" and launcher.exe
;   OUT_FILE      installer path (default PWF-Agent-Setup-<version>.exe)
;   APP_ICON      optional .ico for the installer and shortcuts

Unicode true
SetCompressor /SOLID lzma
ManifestDPIAware true
RequestExecutionLevel user
CRCCheck on

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "FileFunc.nsh"
!include "WinVer.nsh"

!ifndef APP_VERSION
  !error "APP_VERSION is required: makensis -DAPP_VERSION=0.5.0 ..."
!endif
!ifndef SOURCE_DIR
  !error "SOURCE_DIR is required: directory holding the staged build"
!endif
!ifndef OUT_FILE
  !define OUT_FILE "PWF-Agent-Setup-${APP_VERSION}.exe"
!endif
!if /FileExists "${SOURCE_DIR}\PWF Agent.exe"
!else
  !error "SOURCE_DIR must contain 'PWF Agent.exe'"
!endif
!if /FileExists "${SOURCE_DIR}\launcher.exe"
!else
  !error "SOURCE_DIR must contain 'launcher.exe'"
!endif

!ifdef APP_ICON
  !define MUI_ICON "${APP_ICON}"
  !define MUI_UNICON "${APP_ICON}"
!else
  !if /FileExists "${__FILEDIR__}\windows\icon.ico"
    !define MUI_ICON "${__FILEDIR__}\windows\icon.ico"
    !define MUI_UNICON "${__FILEDIR__}\windows\icon.ico"
  !endif
!endif

Name "PWF Agent"
BrandingText "PWF Agent ${APP_VERSION}"
OutFile "${OUT_FILE}"
InstallDir "$LOCALAPPDATA\Programs\PWF Agent"
InstallDirRegKey HKCU "Software\PWF\Agent" "InstallDir"
ShowInstDetails show
ShowUninstDetails show

!define UNINST_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\PWFAgent"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "SimpChinese"
!insertmacro MUI_LANGUAGE "English"

Function .onInit
  ${IfNot} ${AtLeastWin10}
    MessageBox MB_OK|MB_ICONSTOP "PWF Agent 需要 Windows 10 或更高版本。$\r$\nPWF Agent requires Windows 10 or later."
    Abort
  ${EndIf}
FunctionEnd

; Returns whether the Evergreen WebView2 runtime is present in $0.
Function CheckWebView2
  StrCpy $0 ""
  SetRegView 64
  ReadRegStr $1 HKLM "SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
  ReadRegStr $2 HKCU "Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
  SetRegView 32
  ReadRegStr $3 HKLM "SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
  SetRegView lastused
  ${If} $1 != ""
  ${AndIf} $1 != "0.0.0.0"
    StrCpy $0 $1
  ${ElseIf} $2 != ""
  ${AndIf} $2 != "0.0.0.0"
    StrCpy $0 $2
  ${ElseIf} $3 != ""
  ${AndIf} $3 != "0.0.0.0"
    StrCpy $0 $3
  ${EndIf}
FunctionEnd

Function InstallWebView2
  ; The bootstrapper is only staged when the release bundles it.
  InitPluginsDir
  !if /FileExists "${SOURCE_DIR}\MicrosoftEdgeWebview2Setup.exe"
    File /oname=$PLUGINSDIR\MicrosoftEdgeWebview2Setup.exe "${SOURCE_DIR}\MicrosoftEdgeWebview2Setup.exe"
    DetailPrint "安装 WebView2 运行时…"
    ExecWait '"$PLUGINSDIR\MicrosoftEdgeWebview2Setup.exe" /silent /install' $0
    ${If} $0 != 0
      DetailPrint "WebView2 安装返回代码 $0（如系统已内置可忽略）"
    ${EndIf}
  !else
    ${If} ${Silent}
      DetailPrint "未随包提供 WebView2 引导程序；假定系统已安装 WebView2 运行时。"
    ${Else}
      MessageBox MB_OK|MB_ICONEXCLAMATION "未随安装包提供 WebView2 运行时。$\r$\n请先从 Microsoft 安装 WebView2 Evergreen Runtime 后再启动 PWF Agent。"
    ${EndIf}
  !endif
FunctionEnd

Section "PWF Agent" SEC_APP
  ; Every release lives in its own version directory so an update can install
  ; while the old build is still running, and a crash rollback only flips
  ; current.txt.
  SetOutPath "$INSTDIR\${APP_VERSION}"
  File "${SOURCE_DIR}\PWF Agent.exe"
  !if /FileExists "${SOURCE_DIR}\pwf-agent.exe"
    File "${SOURCE_DIR}\pwf-agent.exe"
  !endif
  FileOpen $9 "$INSTDIR\.pwf-install" w
  FileWrite $9 "PWF Agent$\r$\n"
  FileClose $9

  !if /FileExists "${SOURCE_DIR}\tools\exiftool.exe"
    SetOutPath "$INSTDIR\${APP_VERSION}\tools"
    File "${SOURCE_DIR}\tools\exiftool.exe"
  !endif
  !if /FileExists "${SOURCE_DIR}\tools\ffmpeg.exe"
    SetOutPath "$INSTDIR\${APP_VERSION}\tools"
    File "${SOURCE_DIR}\tools\ffmpeg.exe"
  !endif

  !if /FileExists "${SOURCE_DIR}\licenses\THIRD-PARTY-NOTICES.txt"
    SetOutPath "$INSTDIR\licenses"
    File "${SOURCE_DIR}\licenses\THIRD-PARTY-NOTICES.txt"
  !endif
  !if /FileExists "${SOURCE_DIR}\licenses\exiftool-LICENSE.txt"
    SetOutPath "$INSTDIR\licenses"
    File "${SOURCE_DIR}\licenses\exiftool-LICENSE.txt"
  !endif
  !if /FileExists "${SOURCE_DIR}\licenses\ffmpeg-LICENSE.txt"
    SetOutPath "$INSTDIR\licenses"
    File "${SOURCE_DIR}\licenses\ffmpeg-LICENSE.txt"
  !endif

  SetOutPath "$INSTDIR"
  File "${SOURCE_DIR}\launcher.exe"

  ; Point the launcher at this version only after its files are on disk.
  FileOpen $0 "$INSTDIR\current.txt.new" w
  FileWrite $0 "${APP_VERSION}$\r$\n"
  FileClose $0
  Delete "$INSTDIR\current.txt"
  Rename "$INSTDIR\current.txt.new" "$INSTDIR\current.txt"

  Call CheckWebView2
  ${If} $0 == ""
    ${If} ${Silent}
      Call InstallWebView2
    ${Else}
      MessageBox MB_YESNO|MB_ICONQUESTION "未检测到 WebView2 运行时（PWF Agent 的界面依赖它）。现在安装吗？" IDNO webview_done
      Call InstallWebView2
    ${EndIf}
    webview_done:
  ${EndIf}

  WriteUninstaller "$INSTDIR\uninstall.exe"

  CreateDirectory "$SMPROGRAMS\PWF Agent"
  CreateShortcut "$SMPROGRAMS\PWF Agent\PWF Agent.lnk" "$INSTDIR\launcher.exe" "" "$INSTDIR\launcher.exe" 0
  CreateShortcut "$SMPROGRAMS\PWF Agent\卸载 PWF Agent.lnk" "$INSTDIR\uninstall.exe"
  CreateShortcut "$DESKTOP\PWF Agent.lnk" "$INSTDIR\launcher.exe" "" "$INSTDIR\launcher.exe" 0

  WriteRegStr HKCU "Software\PWF\Agent" "InstallDir" "$INSTDIR"
  WriteRegStr HKCU "Software\PWF\Agent" "Version" "${APP_VERSION}"

  WriteRegStr HKCU "${UNINST_KEY}" "DisplayName" "PWF Agent"
  WriteRegStr HKCU "${UNINST_KEY}" "DisplayVersion" "${APP_VERSION}"
  WriteRegStr HKCU "${UNINST_KEY}" "Publisher" "Photography Workflow"
  WriteRegStr HKCU "${UNINST_KEY}" "DisplayIcon" "$INSTDIR\launcher.exe"
  WriteRegStr HKCU "${UNINST_KEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKCU "${UNINST_KEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegDWORD HKCU "${UNINST_KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINST_KEY}" "NoRepair" 1
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  IntFmt $0 "0x%08X" $0
  WriteRegDWORD HKCU "${UNINST_KEY}" "EstimatedSize" "$0"
SectionEnd

Section "Uninstall"
  Delete "$SMPROGRAMS\PWF Agent\PWF Agent.lnk"
  Delete "$SMPROGRAMS\PWF Agent\卸载 PWF Agent.lnk"
  RMDir "$SMPROGRAMS\PWF Agent"
  Delete "$DESKTOP\PWF Agent.lnk"

  DeleteRegKey HKCU "${UNINST_KEY}"
  DeleteRegKey HKCU "Software\PWF\Agent"

  ; Only remove the tree when this directory was created by this installer;
  ; a user-chosen parent folder must never be wiped by accident.
  IfFileExists "$INSTDIR\.pwf-install" 0 uninstall_files_only
  RMDir /r "$INSTDIR"
  Goto uninstall_done

  uninstall_files_only:
  Delete "$INSTDIR\launcher.exe"
  Delete "$INSTDIR\current.txt"
  Delete "$INSTDIR\uninstall.exe"
  Delete "$INSTDIR\.pwf-install"

  uninstall_done:
  ; Imported photos, the state database and settings live under
  ; %LOCALAPPDATA%\PWF\agent and are intentionally kept.
  DetailPrint "用户数据（状态数据库、设置与导入记录）保留在 %LOCALAPPDATA%\PWF\agent。"
SectionEnd
