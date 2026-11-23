«Связь Есть?» для компьютеров — агент @VERSION@ (Linux, @ARCH@)
================================================================

Агент берёт задания DPI//Checker и выполняет их через мобильный модем
(USB-модем, модем HiLink, телефон в режиме USB-модема). Связь с сервером
он держит через Ethernet или Wi-Fi, а модем использует для связи только
если больше ничего не работает.

Быстрая установка — на любом Linux (Debian, Ubuntu, Raspberry Pi OS, Fedora,
Arch, openSUSE, Alpine, Void, Gentoo ...). Docker или служба systemd / OpenRC /
runit / SysV init выбирается само, ModemManager ставится, если его нет:
  curl -fsSL https://github.com/victor50691/svyazest-app/releases/download/desktop/install.sh | sudo sh
Нет curl — то же через wget:
  wget -qO- https://github.com/victor50691/svyazest-app/releases/download/desktop/install.sh | sudo sh
Программы собраны статически: библиотеки дистрибутива им не нужны.

Что в архиве
  svyazest-agent           сам агент
  xray, xray-next          xray-core 26.3.27 и 26.10.31 — для проверки VPN-ключей
  svyazest-agent.service   служба systemd
  openrc/, runit/, sysv/   службы для OpenRC, runit и SysV init
  logrotate/               чистка журнала (OpenRC, SysV init)

Установка вручную
  sudo install -d /usr/lib/svyazest-agent
  sudo install -m 755 svyazest-agent xray xray-next /usr/lib/svyazest-agent/
  sudo ln -sf /usr/lib/svyazest-agent/svyazest-agent /usr/bin/svyazest-agent
  sudo install -m 644 svyazest-agent.service /etc/systemd/system/
  sudo systemctl daemon-reload
  sudo systemctl enable --now svyazest-agent

  Без systemd — вместо последних трёх строк:
  OpenRC (Alpine, Gentoo):
    sudo install -m 755 openrc/svyazest-agent /etc/init.d/
    sudo install -m 644 openrc/svyazest-agent.confd /etc/conf.d/svyazest-agent
    sudo rc-update add svyazest-agent default && sudo rc-service svyazest-agent start
  runit (Void):
    sudo install -d /etc/sv/svyazest-agent/log
    sudo install -m 755 runit/run runit/finish /etc/sv/svyazest-agent/
    sudo install -m 755 runit/log/run /etc/sv/svyazest-agent/log/
    sudo ln -s /etc/sv/svyazest-agent /var/service/
  SysV init (Devuan и др.):
    sudo install -m 755 sysv/svyazest-agent /etc/init.d/
    sudo update-rc.d svyazest-agent defaults && sudo /etc/init.d/svyazest-agent start

Меню агента (вход, состояние, служба, журнал, пароль страницы, обновление)
  sudo svyazest-agent
  Вход отдельной командой: sudo svyazest-agent login

Веб-страница агента
  http://<адрес компьютера>:8780  (на нём самом: http://127.0.0.1:8780)

Модем
  Для обычных USB-модемов нужен ModemManager (агент увидит оператора, сигнал
  и DNS модема) — install.sh ставит его сам. Вручную:
    Debian / Ubuntu / Raspberry Pi OS   sudo apt install modemmanager
    Fedora                              sudo dnf install ModemManager
    Arch / Manjaro                      sudo pacman -S modemmanager
    openSUSE                            sudo zypper install ModemManager
    Alpine                              sudo apk add modemmanager dbus
    Void                                sudo xbps-install -S ModemManager dbus
  и включите его службу (systemctl enable --now ModemManager и т. п.).
  HiLink-модемы (Huawei E3372h и др.) и телефон в режиме USB-модема работают
  и без него.

Полезное
  sudo systemctl status svyazest-agent     состояние службы (systemd)
  journalctl -u svyazest-agent -f          журнал (systemd)
  /var/log/svyazest-agent.log              журнал (OpenRC, SysV init)
  /var/log/svyazest-agent/current          журнал (runit)
  sudo svyazest-agent                      меню агента
  sudo svyazest-agent status               состояние агента
  Данные (вход, журнал): /var/lib/svyazest-agent
  Свои флаги службы: /etc/default/svyazest-agent (SVYAZEST_ARGS)
