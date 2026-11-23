; «Связь Есть?» — установщик агента для Windows (NSIS 3, Unicode).
; Собирается build.sh:
;   makensis -DVERSION=1.0.1 -DSRCDIR=<папка с exe> -DOUTFILE=<setup.exe> installer.nsi
;
; Ставит в Program Files\Svyaz Est, регистрирует и запускает службу
; командами самого агента («service install» / «service start»), ярлык в
; меню «Пуск» открывает страницу агента http://127.0.0.1:8780/.
; Данные агента — %ProgramData%\SvyazEst, при обновлении не трогаются.

Unicode true
ManifestDPIAware true
SetCompressor /SOLID lzma
RequestExecutionLevel admin

!include "MUI2.nsh"
!include "x64.nsh"
!include "LogicLib.nsh"

!ifndef VERSION
  !define VERSION "1.0.1"
!endif
!define APPNAME   "Связь Есть?"
; «?» нельзя в именах файлов Windows — в ярлыке полноширинный «？».
!define LNKNAME   "Связь Есть？"
!define DIRNAME   "Svyaz Est"
!define EXE       "svyazest-agent.exe"
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\SvyazEstAgent"
!define UIURL     "http://127.0.0.1:8780/"

Name "${APPNAME} ${VERSION}"
OutFile "${OUTFILE}"
InstallDir "$PROGRAMFILES64\${DIRNAME}"
InstallDirRegKey HKLM "${UNINSTKEY}" "InstallLocation"
BrandingText "${APPNAME} ${VERSION}"

VIProductVersion "${VERSION}.0"
VIAddVersionKey /LANG=1049 "ProductName" "${APPNAME}"
VIAddVersionKey /LANG=1049 "FileDescription" "Установщик агента «Связь Есть?»"
VIAddVersionKey /LANG=1049 "FileVersion" "${VERSION}"
VIAddVersionKey /LANG=1049 "ProductVersion" "${VERSION}"
VIAddVersionKey /LANG=1049 "CompanyName" "DPI//Checker"
VIAddVersionKey /LANG=1049 "LegalCopyright" "DPI//Checker"

!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_RUN
!define MUI_FINISHPAGE_RUN_TEXT "Открыть страницу агента"
!define MUI_FINISHPAGE_RUN_FUNCTION OpenUI
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "Russian"

Function .onInit
  ${IfNot} ${RunningX64}
    MessageBox MB_ICONSTOP "Нужна 64-битная Windows 10 или 11."
    Abort
  ${EndIf}
  SetRegView 64
FunctionEnd

Function un.onInit
  SetRegView 64
FunctionEnd

Function OpenUI
  ExecShell "open" "${UIURL}"
FunctionEnd

; Агент сам управляет своей службой; код возврата != 0 — показать в журнале.
!macro AgentCmd ARGS
  nsExec::ExecToLog '"$INSTDIR\${EXE}" ${ARGS}'
  Pop $0
  DetailPrint "svyazest-agent ${ARGS}: код $0"
!macroend

Section "Агент" SecMain
  SectionIn RO
  SetOutPath "$INSTDIR"

  ; Обновление: старую службу остановить и снять, иначе exe занят, а
  ; «service install» не перерегистрирует существующую службу.
  ${If} ${FileExists} "$INSTDIR\${EXE}"
    DetailPrint "Останавливаю прежнюю версию..."
    !insertmacro AgentCmd "service stop"
    !insertmacro AgentCmd "service uninstall"
    Sleep 1000
  ${EndIf}

  File "${SRCDIR}\${EXE}"
  File "${SRCDIR}\xray.exe"
  File "${SRCDIR}\xray-next.exe"
  File "${SRCDIR}\README.txt"
  WriteUninstaller "$INSTDIR\uninstall.exe"

  !insertmacro AgentCmd "service install"
  ${If} $0 != 0
    MessageBox MB_ICONEXCLAMATION "Не удалось зарегистрировать службу «SvyazEstAgent» (код $0).$\r$\nПодробности — в журнале установки."
  ${EndIf}
  !insertmacro AgentCmd "service start"

  SetShellVarContext all
  ; Ярлык открывает страницу агента в браузере по умолчанию.
  CreateShortCut "$SMPROGRAMS\${LNKNAME}.lnk" "$WINDIR\explorer.exe" '"${UIURL}"' "$INSTDIR\${EXE}" 0

  WriteRegStr HKLM "${UNINSTKEY}" "DisplayName" "${APPNAME}"
  WriteRegStr HKLM "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKLM "${UNINSTKEY}" "Publisher" "DPI//Checker"
  WriteRegStr HKLM "${UNINSTKEY}" "URLInfoAbout" "https://github.com/victor50691/svyazest-app/releases/tag/desktop"
  WriteRegStr HKLM "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKLM "${UNINSTKEY}" "DisplayIcon" "$INSTDIR\${EXE}"
  WriteRegStr HKLM "${UNINSTKEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKLM "${UNINSTKEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegDWORD HKLM "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD HKLM "${UNINSTKEY}" "NoRepair" 1
  WriteRegDWORD HKLM "${UNINSTKEY}" "EstimatedSize" 90000
SectionEnd

Section "Uninstall"
  !insertmacro AgentCmd "service stop"
  !insertmacro AgentCmd "service uninstall"
  Sleep 1000

  Delete "$INSTDIR\${EXE}"
  Delete "$INSTDIR\xray.exe"
  Delete "$INSTDIR\xray-next.exe"
  Delete "$INSTDIR\README.txt"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"

  SetShellVarContext all
  Delete "$SMPROGRAMS\${LNKNAME}.lnk"
  DeleteRegKey HKLM "${UNINSTKEY}"

  ; Вход партнёра и журнал — только с согласия (при тихом удалении остаются).
  ; (SetShellVarContext all выше: $APPDATA = C:\ProgramData)
  MessageBox MB_YESNO|MB_ICONQUESTION "Удалить и данные агента (вход, журнал) из $APPDATA\SvyazEst?" /SD IDNO IDNO keep
    RMDir /r "$APPDATA\SvyazEst"
  keep:
SectionEnd
