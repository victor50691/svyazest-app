«Связь Есть?» для компьютеров — агент @VERSION@ (Windows 10/11, 64 бит)
=====================================================================

Проще всего — установщик svyazest-agent-@VERSION@-windows-amd64-setup.exe:
он ставит агент в «Program Files\Svyaz Est», регистрирует службу Windows
«SvyazEstAgent» (запускается сама при включении компьютера) и добавляет
в меню «Пуск» ярлык «Связь Есть？», открывающий страницу агента.

Если без установщика (из этого архива), в командной строке от имени
администратора, в папке с файлами:
  svyazest-agent.exe service install
  svyazest-agent.exe service start
Затем откройте в браузере http://127.0.0.1:8780/ и войдите.
Удаление службы:
  svyazest-agent.exe service stop
  svyazest-agent.exe service uninstall

Что в архиве
  svyazest-agent.exe       сам агент
  xray.exe, xray-next.exe  xray-core 26.3.27 и 26.10.31 — для проверки VPN-ключей

Данные агента (вход, журнал): %ProgramData%\SvyazEst
