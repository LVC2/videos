# Debian 13 deployment

Целевая схема сервера:

- Hostname: `video-server`
- IPv4: `10.120.1.24/24`
- Gateway: определяется автоматически из текущего default route
- Firewall: UFW
- SSH: 22/tcp
- Video Core: 8090/tcp
- Service user: `videos`
- Application directory: `/opt/videos`
- FFmpeg: system package
- Core: systemd service

## Установка базовой системы

На консоли Debian/ESXi:

```bash
cd /tmp
git clone https://github.com/LVC2/videos.git
cd videos
chmod +x deploy/debian13/setup.sh
sudo ./deploy/debian13/setup.sh
```

Скрипт не делает reboot автоматически. Это сделано специально, чтобы не потерять удалённую SSH-сессию.

После проверки:

```bash
reboot
```

После reboot:

```ip -br a
ip route
ufw status verbose
```

Полная инструкция установки системного стека: [`STACK.md`](STACK.md).

## Развёртывание приложения

Собрать бинарник:

```bash
cd /opt/videos
go build -o video-core .
chown videos:videos /opt/videos/video-core
```

Скопировать `web/` и `config.json`, затем:

```bash
systemctl restart video-core
journalctl -u video-core -f
```

## Порты

Основные правила UFW описаны в [`STACK.md`](STACK.md). На первом этапе приложение Video Core ещё не открываем.

RTSP от камер сервер получает исходящим соединением, поэтому inbound RTSP-порты открывать не нужно.

Когда перейдём на собственный WebRTC transport, добавим необходимые UDP/TCP-порты отдельно.
