# Debian 13 — базовая установка Video Server

Сервер:

- IP: `10.120.1.24/24`
- Gateway: `10.120.1.1`
- SSH: `2222/tcp`
- Web: `80/tcp`
- phpMyAdmin: `10.120.1.24:8081`
- Video Core: `8090/tcp` — позже, после установки приложения
- стандартный web-root: `/var/www/html`
- PHP: Debian 13 / PHP 8.4
- MariaDB: Debian 13 штатный пакет
- Node.js + npm: Debian 13 штатные пакеты
- go2rtc: официальный Linux amd64 binary

PHP 8.4 и phpMyAdmin 5.2.2 доступны штатно в Debian 13 (trixie). citeturn620408search2turn620408search0
go2rtc v1.9.14 — актуальный проверенный release для этого проекта. citeturn620408search1

## 1. Обновление системы

```bash
sudo apt update
sudo apt full-upgrade -y
```

## 2. Основной стек

```bash
sudo apt install -y \
    nginx \
    mariadb-server \
    mariadb-client \
    php8.4-fpm \
    php8.4-cli \
    php8.4-mysql \
    php8.4-curl \
    php8.4-mbstring \
    php8.4-xml \
    php8.4-zip \
    php8.4-gd \
    php8.4-intl \
    php8.4-opcache \
    phpmyadmin \
    git \
    nodejs \
    npm \
    curl \
    wget \
    unzip \
    ca-certificates
```

При установке `phpmyadmin`:

- web server: **ничего не выбираем**;
- `dbconfig-common`: можно выбрать **No** — базу phpMyAdmin настроим вручную через MariaDB.

## 3. Включить сервисы

```bash
sudo systemctl enable --now nginx
sudo systemctl enable --now mariadb
sudo systemctl enable --now php8.4-fpm
```

Проверка:

```bash
systemctl --no-pager --type=service --state=running | grep -E 'nginx|mariadb|php8.4-fpm'
```

## 4. Web-root

Оставляем стандартный Debian путь:

```text
/var/www/html
```

Права:

```bash
sudo chown -R www-data:www-data /var/www/html
sudo find /var/www/html -type d -exec chmod 755 {} \;
sudo find /var/www/html -type f -exec chmod 644 {} \;
```

Тестовый PHP:

```bash
echo '<?php phpinfo();' | sudo tee /var/www/html/index.php >/dev/null
```

## 5. Nginx для основного сайта

Удаляем стандартный сайт:

```bash
sudo rm -f /etc/nginx/sites-enabled/default
```

Создаём:

```bash
sudo nano /etc/nginx/sites-available/videos
```

Содержимое:

```nginx
server {
    listen 80 default_server;
    listen [::]:80 default_server;

    server_name 10.120.1.24 _;

    root /var/www/html;
    index index.php index.html;

    location / {
        try_files $uri $uri/ /index.php?$query_string;
    }

    location ~ \.php$ {
        include snippets/fastcgi-php.conf;
        fastcgi_pass unix:/run/php/php8.4-fpm.sock;
    }

    location ~ /\. {
        deny all;
    }
}
```

Активируем:

```bash
sudo ln -sf /etc/nginx/sites-available/videos /etc/nginx/sites-enabled/videos
sudo nginx -t
sudo systemctl reload nginx
```

Проверка:

```bash
curl -I http://10.120.1.24/
```

## 6. phpMyAdmin на отдельном порту 8081

Никакого `/phpmyadmin` внутри основного сайта.

Создаём отдельный Nginx server block:

```bash
sudo nano /etc/nginx/sites-available/phpmyadmin
```

```nginx
server {
    listen 10.120.1.24:8081;
    server_name _;

    root /usr/share/phpmyadmin;
    index index.php;

    location / {
        try_files $uri $uri/ /index.php?$query_string;
    }

    location ~ \.php$ {
        include snippets/fastcgi-php.conf;
        fastcgi_pass unix:/run/php/php8.4-fpm.sock;
    }

    location ~ /\. {
        deny all;
    }
}
```

Активируем:

```bash
sudo ln -sf /etc/nginx/sites-available/phpmyadmin /etc/nginx/sites-enabled/phpmyadmin
sudo nginx -t
sudo systemctl reload nginx
```

Открывать:

```text
http://10.120.1.24:8081/
```

## 7. UFW

Основной принцип: наружу не открываем ничего лишнего.

```bash
sudo ufw default deny incoming
sudo ufw default allow outgoing
```

SSH:

```bash
sudo ufw allow 2222/tcp comment 'SSH'
```

Web:

```bash
sudo ufw allow 80/tcp comment 'Nginx HTTP'
sudo ufw allow 443/tcp comment 'Nginx HTTPS'
```

phpMyAdmin доступен только из локальной сети:

```bash
sudo ufw allow from 10.120.1.0/24 to 10.120.1.24 port 8081 proto tcp comment 'phpMyAdmin LAN'
```

Пока Video Core не установлен, порт 8090 не открываем.

Включить:

```bash
sudo ufw --force enable
sudo ufw status numbered
```

## 8. MariaDB

```bash
sudo mariadb-secure-installation
```

Для нашего сервера:

- удалить anonymous users — Yes;
- запретить remote root login — Yes;
- удалить test database — Yes;
- reload privilege tables — Yes.

Проверка:

```bash
sudo mariadb -e 'SELECT VERSION();'
```

## 9. Node / npm / Git

```bash
node -v
npm -v
git --version
```

На этом этапе ставим только системные пакеты Debian. Node ecosystem проекта пока не разворачиваем.

## 10. go2rtc

Для x86_64:

```bash
cd /tmp
wget https://github.com/AlexxIT/go2rtc/releases/download/v1.9.14/go2rtc_linux_amd64
sudo install -m 0755 go2rtc_linux_amd64 /usr/local/bin/go2rtc
/usr/local/bin/go2rtc --version
```

go2rtc v1.9.14 официально опубликован как release 19 января 2026 года. citeturn620408search1

Конфиг:

```bash
sudo mkdir -p /etc/go2rtc
sudo nano /etc/go2rtc/go2rtc.yaml
```

Пока:

```yaml
api:
  listen: ":1984"

rtsp:
  listen: ":8554"

webrtc:
  listen: ":8555"
```

## 11. systemd для go2rtc

```bash
sudo nano /etc/systemd/system/go2rtc.service
```

```ini
[Unit]
Description=go2rtc media server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/go2rtc -config /etc/go2rtc/go2rtc.yaml
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
```

Запуск:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now go2rtc
```

Проверка:

```bash
systemctl status go2rtc --no-pager
curl http://127.0.0.1:1984/api
```

## 12. UFW для go2rtc

Доступ к go2rtc только из нашей LAN:

```bash
sudo ufw allow from 10.120.1.0/24 to 10.120.1.24 port 1984 proto tcp comment 'go2rtc API'
sudo ufw allow from 10.120.1.0/24 to 10.120.1.24 port 8554 proto tcp comment 'go2rtc RTSP'
sudo ufw allow from 10.120.1.0/24 to 10.120.1.24 port 8555 proto tcp comment 'go2rtc WebRTC TCP'
sudo ufw allow from 10.120.1.0/24 to 10.120.1.24 port 8555 proto udp comment 'go2rtc WebRTC UDP'
```

Проверить:

```bash
sudo ufw status numbered
sudo ss -lntup | grep -E ':80 |:8081 |:1984 |:8554 |:8555 |:2222 '
```

## Итоговая схема

```text
10.120.1.24

SSH          :2222  ← LAN
Nginx        :80     ← сайт /var/www/html
phpMyAdmin   :8081   ← LAN only
go2rtc       :1984  ← LAN only
go2rtc RTSP  :8554   ← LAN only
go2rtc WebRTC :8555  ← LAN only

MariaDB      :3306   ← наружу НЕ открывать
PHP-FPM      :9000   ← наружу НЕ открывать
```

После этого базовый Debian-сервер готов. Сам `LVC2/videos` и его собственный Video Core разворачиваем уже следующим этапом.
