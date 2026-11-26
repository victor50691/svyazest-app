# «Связь Есть?» для компьютеров

Агент для Linux (включая Raspberry Pi и Docker) и Windows. Делает то же, что
Android-приложение: берёт задания DPI//Checker (домены, IP, подсети /24,
VPN-ключи) и выполняет их через мобильную сеть партнёра — USB-модем, модем
HiLink или телефон в режиме USB-модема. Связь с сервером агент держит через
Ethernet или Wi-Fi, а модем использует для неё, только если больше ничего не
работает.

```
компьютер ── Ethernet / Wi-Fi ── сервер DPI//Checker (задания, результаты)
          └─ модем (SIM) ─────── сами проверки
```

Готовые файлы — в релизе
[`desktop`](https://github.com/victor50691/svyazest-app/releases/tag/desktop)
(он всегда содержит текущую версию).

## Установка

**Linux и Raspberry Pi** — одна команда (она же обновляет):

```bash
curl -fsSL https://github.com/victor50691/svyazest-app/releases/download/desktop/install.sh | sudo sh
```

`install.sh` сам выбирает пакет `.deb`, архив или Docker и службу
(systemd, OpenRC, runit, SysV init), ставит ModemManager, если его нет, и
сверяет файлы по `SHA256SUMS`. После установки:

```bash
sudo svyazest-agent          # меню: вход, состояние, служба, журнал, обновление
```

Страница агента — `http://<адрес компьютера>:8780`, пароль страницы — код
настройки из меню (или свой: `sudo svyazest-agent ui-password`).

**Windows** —
[svyazest-agent-windows-setup.exe](https://github.com/victor50691/svyazest-app/releases/download/desktop/svyazest-agent-windows-setup.exe):
служба `SvyazEstAgent`, страница на `http://127.0.0.1:8780`.

## Команды

```
svyazest-agent                 меню (Linux) / справка
svyazest-agent run             работа на переднем плане (так её запускает служба)
svyazest-agent login           вход через Telegram
svyazest-agent status          подробное состояние
svyazest-agent logout          выход из аккаунта
svyazest-agent ui-password     свой пароль страницы агента
svyazest-agent service install|uninstall|start|stop
svyazest-agent version
```

Общие флаги: `--data-dir`, `--listen`, `--api`, `--modem-iface`, `--xray`,
`--xray-next`, `--verbose`. Переменные окружения: `SVYAZEST_DATA`,
`SVYAZEST_LISTEN`, `SVYAZEST_API`.

## Как устроено

| Пакет | Что делает |
|---|---|
| `cmd/svyazest-agent` | команды и интерактивное меню (Linux) |
| `internal/api` | клиент `/executor/v1/*` сервера DPI//Checker, ключ устройства и подписи запросов |
| `internal/runner` | рабочий цикл: heartbeat, проверка «белых списков», получение заданий, результаты с повторами, паузы по ответам сервера |
| `internal/checks` | сами проверки (порт из Android-приложения): домены, IP, /24, VPN-ключи через xray-core |
| `internal/netbind` | привязка проверок к интерфейсу модема и канал связи с сервером в обход модема |
| `internal/modem` | поиск модема: ModemManager, HiLink, телефон по USB; оператор, сигнал, SIM |
| `internal/store` | данные на диске и журнал; секреты — файлы 0600 (Linux) или DPAPI (Windows) |
| `internal/svc` | работа службой: systemd / OpenRC / runit / SysV init, служба Windows |
| `internal/ui` | своя веб-страница на :8780 (без CDN) и её API, обновления через SSE |
| `cmd/mock-executor` | имитация сервера для проверки агента без настоящей очереди |
| `packaging/` | install.sh, .deb, службы, Docker, установщик Windows (NSIS) |

Цели заданий (домены, адреса, ключи) не попадают ни в журнал, ни на
страницу агента, ни в диагностические логи.

Для VPN-ключей агент запускает xray-core двух версий — 26.3.27 (та же, что в
Android-приложении) и 26.10.31; какую взять, решает поле ключа.

## Сборка

Нужен Go (версия в `go.mod`). Проверка и тесты:

```bash
go vet ./...
GOOS=windows go vet ./...
go test ./...
```

Тесты, которым нужны сеть, xray или root, по умолчанию пропускаются и
включаются переменными (`SVZ_NET_TESTS`, `SVZ_XRAY`, `NETBIND_LIVE`,
`SVYAZEST_HOTPLUG_TEST`).

Полный выпуск (все архитектуры, .deb, архивы, Docker-образы, установщик
Windows, `SHA256SUMS`) собирает `./build.sh` в `dist/`:

```bash
./build.sh                    # всё
SKIP_DOCKER=1 ./build.sh      # без образов Docker
VERSION=1.0.10 ./build.sh
```

Кроме Go понадобятся `git`, `dpkg-deb`, `makensis` (nsis), `zip`, для
образов — `docker` с buildx и qemu-user-binfmt. Исходники Xray-core обеих
версий скрипт клонирует сам с github.com/XTLS/Xray-core и проверяет их
коммиты.

Запуск против имитации сервера:

```bash
go run ./cmd/mock-executor -listen 127.0.0.1:18080 &
go run ./cmd/svyazest-agent run --api http://127.0.0.1:18080 --data-dir /tmp/svz
```

## Лицензия

MIT — см. [LICENSE](../LICENSE) в корне репозитория. Xray-core —
MPL-2.0, см. [third_party/xray-core](../third_party/xray-core).
