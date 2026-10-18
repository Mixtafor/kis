### Черноморцев Ярослав Сергеевич

### Задача 6   Системное программирование "Издание"


### 1) как запустить
#### сервер
либо
```sh
cp .env.example .env
```
что скопирует дефолтные значения ENV переменных

либо нужно заполнить .env файл переменными окружения, их примеры можно посмотреть в .env.example

HTTP_PORT — порт сервера, EDITION_URL — адрес для клиента. POSTGRES_USER, POSTGRES_PASSWORD и POSTGRES_DB — настройки БД, DB_PORT — её порт на хосте. DATABASE_URL нужен для Go на хосте

далее
```sh
make up
make seed
```
make up запускает БД, применяет миграцию и поднимает сервер. make seed добавляет демо-клиентов. По умолчанию сервер на http://localhost:8080, Swagger  на http://localhost:8080/docs - описание ручек

#### клиент cli

```sh
make build
set -a
. ./.env
set +a
export EDITION_CLIENT_ID=tonny
export EDITION_API_KEY="$DEMO_API_KEY"
```
export EDITION_CLIENT_ID=tonny выбирает в текущей сесии пользователя тонни

export EDITION_API_KEY="$DEMO_API_KEY" задаёт его API-ключ для входа

переключиться на админа можно так

```sh
export EDITION_CLIENT_ID=editor
export EDITION_API_KEY="$EDITOR_API_KEY"
```
#### что может cli

команды вызываются через bin/client, все параметры можно посмотреть в bin/client help

для пользователя:

bin/client submit — отправить заметку, через --file можно отправить сохранённый JSON

bin/client stats — посмотреть свою статистику

bin/client stories — список новостей с фильтрами по рубрике и состоянию

bin/client story --id ID — посмотреть новость и её авторов

bin/client messages — получить свои поздравления

bin/client ack --id ID — подтвердить получение сообщения

bin/client help — показать команды и параметры

для админа:

bin/client create-client — создать клиента и получить его API-ключ

bin/client rotate-key --id ID — выдать клиенту новый API-ключ

bin/client set-trust — изменить доверие, включить или отключить клиента

bin/client all-stats — посмотреть статистику всех клиентов

bin/client policy — посмотреть настройки публикации

bin/client set-policy — изменить порог доверия, период полураспада и число поздравлений


### 2) пример работы

#### пользователь

тонни отправляет заметку, сахарисса подтверждает эту же новость

```sh
export EDITION_CLIENT_ID=tonny
export EDITION_API_KEY="$DEMO_API_KEY"
bin/client submit --request-id park-1 --category Город --topic Парк --body "Открыли парк"
export EDITION_CLIENT_ID=sacharissa
bin/client submit --request-id park-2 --category Город --topic Парк --body "Подтверждаю открытие"
bin/client stories --state published
bin/client messages
bin/client stats
```

при стандартных настройках их доверия должно хватить для публикации

#### админ

пример редактор смотрит общую статистику, меняет доверие тонни и проверяет настройки публикации

```sh
export EDITION_CLIENT_ID=editor
export EDITION_API_KEY="$EDITOR_API_KEY"
bin/client all-stats
bin/client set-trust --id tonny --trust 0.5
bin/client policy
```


### 3) использованные технологии

Go и net/http — сервер и CLI-клиент

PostgreSQL — хранение данных, pgx — SQL-запросы, golang-migrate — миграции БД

JWT — токен для запросов после входа по API-ключу

log/slog — логи запросов и ошибок в JSON

Docker и Docker Compose — сборка и запуск сервера, БД и миграций в контейнерах

OpenAPI и Swagger UI — описание API и вызов ручек через браузер

testing — unit-тесты на Go, pytest и httpx — E2E-тесты через HTTP

Make — команды сборки и запуска, golangci-lint и govulncheck — проверка кода и поиск уязвимостей

### 4) принятые проектные решения

сервер работает одним приложением, CLI обращается к нему по HTTP и получает JSON
после рукопожатия используется JWT, а роль и активность клиента читаются из БД при каждом запросе, чтобы изменения доступа действовали сразу

для распознавания повторов используются клиент, request_id и хеш данных.
одновременные отправки реджектаться блокировкой строки новости в постгресе. заметка, публикация и поздравления сохраняются одной транзакцией, чтобы изменения применялись вместе

при публикации поздравления записываются в PostgreSQL, даже если клиент сейчас не подключён. команда bin/client messages запрашивает у сервера все неподтверждённые сообщения. чтения не достаточно для аппрува, и сообщение будет возвращаться при следующих запросах, пока клиент не выполнит bin/client ack --id ID. после этого оно исчезнет из списка неподтверждённых

#### 5) зависимости
Нужны Docker Desktop, Make и Go 1.27.2.
