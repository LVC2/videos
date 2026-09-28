# Video Server / Video Core

Минимальный самостоятельный видеосервер:

- Go core: управление камерами и FFmpeg-процессами;
- MariaDB: конфигурация камер;
- RTSP input;
- HLS output;
- Vue 3 UI;
- запуск/остановка каждого потока;
- один процесс FFmpeg на активную камеру;
- без go2rtc.

## Запуск

Конфигурация подключения к MariaDB передаётся через переменную окружения:

```bash
export VIDEOS_DB_DSN='user:password@tcp(127.0.0.1:3306)/videos?parseTime=true&charset=utf8mb4'
go build -o video-core .
./video-core
```

В production переменная задаётся через systemd EnvironmentFile.

Камеры загружаются из таблицы `cameras`. Используются поля `slug`, `name`, `rtsp_url`, `rtsp_username`, `rtsp_password`, `enabled`, `autostart`.

RTSP-учётные данные не возвращаются через `/api/cameras`.

UI: `http://SERVER:8090/`

## Важно

Первая версия намеренно простая. Она не является заменой полноценному WebRTC media server.
Для камер H.264 используется `-c:v copy`, поэтому FFmpeg не перекодирует видео.

Следующие этапы:

1. WebRTC output для низкой задержки.
2. Автоматический reconnect RTSP.
3. Snapshot API.
4. Запись архива.
5. Детектор движения.
6. PTZ/zoom controls.
7. Авторизация и роли.
8. Управление камерами и разделами через UI.
