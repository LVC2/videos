# Video Server / Video Core

Самостоятельный видеосервер на Go + MariaDB, работающий через **go2rtc**.

## Архитектура

```
Hikvision
 ├── Main Stream (101) ──> go2rtc ──> архив при VMD
 └── Sub Stream  (102) ──> go2rtc ──> WebRTC live

Hikvision ISAPI
 └── /ISAPI/Event/notification/alertStream
             │
             └── VMD active/inactive
                     │
                     └── video-core управляет записью
```

Видео не проходит через FFmpeg. Video Core не декодирует и не перекодирует видеопоток.

Для Hikvision используются стандартные RTSP каналы: 101 — основной поток, 102 — дополнительный.

## Что сейчас реализовано

- Go core;
- MariaDB для конфигурации камер;
- go2rtc как media layer;
- автоматическая регистрация Main/Sub RTSP streams в go2rtc;
- WebRTC live через Sub Stream;
- Hikvision Digest Authentication;
- получение VMD событий с камеры;
- запуск записи при `VMD active`;
- остановка записи после `motion_post_seconds`;
- запись Main Stream через go2rtc `/api/stream.mp4`;
- каталог архива: `runtime/recordings/<camera>/<YYYY-MM-DD>/`;
- управление камерами и разделами через UI.

go2rtc предоставляет WHEP WebRTC endpoint `/api/webrtc?src=...` и MP4 progressive stream API; эти интерфейсы используются Video Core вместо HLS/FFmpeg.

Hikvision `alertStream` устанавливает постоянное соединение и передаёт события, включая `VMD` с состояниями `active/inactive`.

## Запуск

Конфигурация MariaDB:

```bash
export VIDEOS_DB_DSN='user:password@tcp(127.0.0.1:3306)/videos?parseTime=true&charset=utf8mb4'
go build -o video-core .
./video-core
```

Production использует systemd EnvironmentFile.

## Конфигурация

`config.json`:

```json
{
  "listen": "127.0.0.1:8090",
  "go2rtc": "http://127.0.0.1:1984",
  "media_dir": "./runtime",
  "motion_post_seconds": 10,
  "db_dsn": ""
}
```

`go2rtc` — HTTP API адрес локального go2rtc.

## Принцип записи

При `VMD active` Video Core открывает Main Stream через go2rtc и сохраняет получаемый MP4-поток в архив.

При `VMD inactive` запись продолжается ещё `motion_post_seconds` секунд.

Сервер не выполняет motion detection и не перекодирует видео.

## Следующий этап

1. проверить реальные VMD события на Hikvision;
2. проверить MP4-файлы после реального движения;
3. добавить таблицу архива и API записей;
4. добавить страницу архива/поиск/скачивание;
5. добавить pre-record buffer без перекодирования;
6. добавить очистку архива по сроку/свободному месту;
7. добавить права доступа.
