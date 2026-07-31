# all-for-one-drive

Подготовка фото/видео к бэкапу и загрузка на несколько облаков через [rclone](https://rclone.org/) с репликацией по эшелонам.

Пайплайн одной команды `convert-and-upload`:

1. PreCheck всех облаков → `availability.log`
2. Конвертация изображений → JPEG
3. Конвертация видео → MP4
4. Архивация (опционально) или staging без архивов
5. Upload реплик по эшелонам в `all-for-one/...`
6. Очистка staging после успеха

Исходники в `image-input` / `video-input` **не удаляются и не перезаписываются**.

---

## Требования

| Инструмент | Зачем | Windows (пример) |
|---|---|---|
| **Go 1.22+** (в репо указан `go 1.26.5`) | сборка | [go.dev/dl](https://go.dev/dl/) |
| **rclone** в `PATH` | облака | `winget install Rclone.Rclone` |
| **ffmpeg** + **ffprobe** в `PATH` | видео | `winget install Gyan.FFmpeg` |
| **7-Zip** (`7z`/`7zz`/`7za`) в `PATH` | архивы | `winget install 7zip.7zip` |

После установки утилит **перезапустите терминал**, чтобы подтянулся `PATH`.

Проверка:

```powershell
go version
rclone version
ffmpeg -version
ffprobe -version
7z
```

---

## Развёртывание с нуля

```powershell
git clone <url-репозитория> all-for-one-drive
cd all-for-one-drive

# зависимости Go
go mod download

# собрать бинарник
mkdir bin -Force
go build -o .\bin\convert-and-upload.exe .\cmd\convert-and-upload

# рабочий конфиг рядом с exe (шаблон в корне репо)
Copy-Item .\all-for-one.config.jsonc .\bin\all-for-one.config.jsonc
```

Каталог `bin/` в `.gitignore` — туда кладутся exe, локальный конфиг с секретами, логи и токены.

Структура рядом с бинарником после первого запуска:

```text
bin/
  convert-and-upload.exe
  all-for-one.config.jsonc      ← ваш конфиг с credentials
  image-input/                  ← сюда кладёте фото
  video-input/                  ← сюда кладёте видео
  image-output/                 ← результат конвертации (временный)
  video-output/
  archive-output/               ← архивы (если archive: true)
  upload-output/                ← staging без архивов
  convert-and-upload.log        ← лог текущего запуска (очищается каждый раз)
  availability.log              ← таблица PreCheck (дописывается)
  *.oauth-token                 ← OAuth refresh tokens (не коммитить)
```

---

## Конфиг

Файл по умолчанию: `all-for-one.config.jsonc` **рядом с exe**.  
Другой путь: `-config <path>`.

Пути внутри конфига относительные — считаются от директории конфига.

Корневой `all-for-one.config.jsonc` в репозитории — шаблон с комментариями и примерами провайдеров. Рабочий экземпляр с реальными ключами держите в `bin/` (он не попадёт в git).

### Глобальные параметры

| Ключ | Смысл |
|---|---|
| `replication` | Сколько копий по эшелонам (по умолчанию `3`, диапазон `1..3`) |
| `echelon` | С какого эшелона начинать (`1..3`). Окно: `echelon .. min(3, echelon+replication-1)` |
| `archive` | `true` — паковать в 7z (для провайдеров с `acceptsArchives: true`); иначе грузить медиа напрямую |

Примеры:

- `replication: 3`, `echelon: 1` → эшелоны **1, 2, 3**
- `replication: 2`, `echelon: 2` → эшелоны **2, 3**
- `replication: 2`, `echelon: 3` → только **3** (без полноценной репликации)

### Провайдер

```jsonc
{
  "name": "cloudflare-r2",
  "type": "s3",                 // тип rclone backend
  "echelon": 1,                 // 1..3
  "priority": 2,                // 1 — раньше; 0 отключён; 101 «заполнен»
  "acceptsArchives": true,
  "maxUsageMB": 9900,           // опционально: жёсткий потолок по rclone size
  "accounts": [{
    "name": "primary",
    "rootPath": "bucket-name",  // bucket / корень; часто обязателен для S3/B2
    "options": { /* ключи rclone */ }
  }]
}
```

Правила приоритета:

- внутри эшелона сначала грузится меньший `priority`;
- `0` и `101` **не** участвуют в upload, но всё равно проходят PreCheck (чтобы аккаунты не «засыпали»);
- внутри одного эшелона один файл уходит только в одну цель (остаток по квоте — следующей по priority);
- пустой эшелон пропускается с предупреждением.

Облачная папка назначения всегда: `all-for-one/<ваши подпапки из input>`.

---

## Провайдеры (кратко)

Шаблоны есть в `all-for-one.config.jsonc`. Ниже — что нужно на практике.

### Google Drive (`type: drive`)

1. Google Cloud Console → OAuth client (Desktop).
2. Включить **Google Drive API**.
3. В конфиг: `client_id`, `client_secret`, опционально `root_folder_id`.
4. `"oauth": { "tokenFile": "./google-drive-personal.oauth-token" }`
5. Первый запуск откроет браузер; refresh token сохранится в файл.

### Box (`type: box`)

Проще без своего Client ID — пустые `options`, OAuth через встроенный клиент rclone:

```jsonc
"oauth": { "tokenFile": "./box-personal.oauth-token" },
"options": {}
```

Свой Box App часто ломается на `redirect_uri` (`127.0.0.1` vs `localhost`).

### Cloudflare R2 (`type: s3`, `provider: Cloudflare`)

Нужны **R2 API Token** (Access Key ID + Secret), не Account API Token `cfat_...`.

```jsonc
"rootPath": "your-bucket",
"options": {
  "provider": "Cloudflare",
  "access_key_id": "...",
  "secret_access_key": "...",
  "endpoint": "https://<ACCOUNT_ID>.r2.cloudflarestorage.com",
  "acl": "private"
}
```

Рекомендуется `maxUsageMB: 9900` (free ~10 GiB).

### Backblaze B2 (`type: b2`)

```jsonc
"rootPath": "bucket-name",
"options": {
  "account": "keyID",
  "key": "applicationKey"
}
```

### Storj (`type: s3`, `provider: Storj`)

```jsonc
"rootPath": "bucket-name",
"options": {
  "provider": "Storj",
  "access_key_id": "...",
  "secret_access_key": "...",
  "endpoint": "https://gateway.storjshare.io"
}
```

`rclone about` у S3/Storj нет — занятость считается через `rclone size`.

### MEGA (`type: mega`)

```jsonc
"options": {
  "user": "email@example.com",
  "pass": "password"   // plaintext; программа передаёт --obscure
}
```

Если включён 2FA — добавьте блок `twoFactor` с Base32-секретом и `"rcloneOption": "2fa"`.

### Cloudinary (`type: cloudinary`)

```jsonc
"acceptsArchives": false,
"options": {
  "cloud_name": "...",
  "api_key": "...",
  "api_secret": "..."
}
```

Архивы не принимает — при `archive: true` для него готовятся прямые JPEG/MP4.

---

## Запуск

Положите файлы:

- фото → `bin/image-input/...` (подпапки сохраняются)
- видео → `bin/video-input/...`

```powershell
cd bin
.\convert-and-upload.exe
# или
.\convert-and-upload.exe -config D:\path\to\all-for-one.config.jsonc
```

Если вход пустой, PreCheck всё равно выполнится и допишет `availability.log`, upload не стартует — это нормально (удобно «пинговать» облака).

### Логи и ошибки

| Файл | Поведение |
|---|---|
| `convert-and-upload.log` | полный лог; **очищается** каждый запуск |
| `availability.log` | таблица квот/статусов; **дописывается** |
| `convert-and-upload.error` | появляется только при ошибке; при старте удаляется |

---

## Типичный минимальный сетап

1. Установить Go, rclone, ffmpeg, 7-Zip.
2. Склонировать репо, `go build` в `bin/`.
3. Скопировать конфиг в `bin/`, раскомментировать/добавить ≥1 провайдера на нужные эшелоны.
4. Выставить `priority` (`1+` = активен, `0` = только PreCheck).
5. Для OAuth-провайдеров пройти браузерный логин при первом запуске.
6. Положить тестовые файлы в `image-input` / `video-input`.
7. Запустить exe и проверить `availability.log` + облачную папку `all-for-one`.

---

## Безопасность

- Не коммитьте `bin/all-for-one.config.jsonc` с ключами, `*.oauth-token`, `*.otp`, логи.
- В шаблонном конфиге в корне репозитория оставляйте плейсхолдеры (`CLIENT_ID`, `ACCESS_KEY`, …).
- Password-поля в JSONC можно писать открытым текстом — rclone создаёт временный config с `--obscure`.

---

## Структура репозитория

```text
cmd/convert-and-upload/     — оркестратор
internal/
  applog/                   — логи, .error, availability.log
  config/                   — загрузка JSONC
  image-converter/
  video-converter/
  archiver/
  rclone/                   — PreCheck / Upload / квоты
  oauth/                    — rclone authorize + token files
  twofactor/                — TOTP для backends вроде MEGA
all-for-one.config.jsonc    — шаблон конфига
```
