# Video Server / Video Core

Минимальный самостоятельный видеосервер:

- Go core: управление камерами и FFmpeg-процессами;
- RTSP input;
- HLS output;
- Vue 3 UI;
- запуск/остановка каждого потока;
- один процесс FFmpeg на активную камеру;
- без go2rtc.

## Запуск

```bash
export VIDEO_RTSP_FRONT='rtsp://USER:PASSWORD@CAMERA/Streaming/Channels/101'
export VIDEO_RTSP_REAR='rtsp://USER:PASSWORD@CAMERA/Streaming/Channels/101'
export VIDEO_RTSP_CARGO='rtsp://USER:PASSWORD@CAMERA/Streaming/Channels/101'
export VIDEO_RTSP_INSIDE='rtsp://USER:PASSWORD@CAMERA/Streaming/Channels/101'
go build -o video-core .
./video-core
```

UI: `http://SERVER:8090/`

## Важно

Первая версия намеренно простая. Она не является заменой полноценному WebRTC media server.
Для камер H.264 используется `-c:v copy`, поэтому FFmpeg не перекодирует видео.

Следующий этап можно сделать поверх этого ядра:

1. WebRTC output для низкой задержки.
2. Автоматический reconnect RTSP.
3. Snapshot API.
4. Запись архива.
5. Детектор движения.
6. PTZ/zoom controls.
7. Авторизация и роли.
8. Хранение конфигурации в MariaDB.
