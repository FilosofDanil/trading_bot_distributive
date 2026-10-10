# trading_bot_distributive

Управление крипто-капиталом и торговыми ботами через Telegram. Binance USDⓈ-M futures, хостинг Railway (Hobby, $5/мес).

Это чистая версия архитектуры. Рабочий код переносится сюда по частям из `Desktop/AI_Projects/Engine`
(`lossguard/`, `Trading_Bot/`, `journal/`). Подробная схема с диаграммой лежит в
`Engine/docs/trading_infrastructure.html` (её стоит перенести в `docs/architecture.html`).

## Что делает система

1. **Следит за счётом**: баланс, equity, PnL за день, просадка от пика. Присылает алерты в Telegram.
2. **Блокирует торговлю**, когда достигнут лимит: ставит ботов на паузу, отменяет ордера, закрывает позиции
   и закрывает любые новые позиции до конца блокировки.
3. **Управляет ботами**: показывает список доступных стратегий, запускает и ставит их на паузу, делает flatten.
4. **Строит отчёты**: по каждой стратегии отдельно и общий со сравнением стратегий, ручная торговля идёт отдельной строкой.
5. **Делает бэкапы базы**: каждый день и по команде. Файл приходит в Telegram и поднимается локально.

## Архитектура

```
Telegram (ты) ──Bot API, long polling──► control ──HTTP, private network──► runner ──► Binance
                                            │  └──────────► Binance (мониторинг счёта, блокировка)
                                            ▼                    │
                                        Postgres ◄───────────────┘
                                            ▲
                                  backup (cron) ──► R2/B2 + Telegram
control ──ping/мин──► healthchecks.io (внешний watchdog)
```

| Сервис | Язык | Роль | Откуда переносим |
|---|---|---|---|
| `control` | Go | Telegram UI, risk guard (раз в 10 с), опрос runner'ов, отчёты, `/backup` | `Engine/lossguard` |
| `runner` | Python | стратегии-плагины, ордера, control API на `[::]:8080` | `Engine/Trading_Bot/swingbot` |
| `backup` | shell / Go | cron `0 3 * * *` UTC: `pg_dump -Fc` → R2/B2 + Telegram | новый |
| Postgres | — | единая база (шаблон Railway, volume 1 GB) | заменяет `state.json` и `bot.db` |

Принципы:

- **Публичных доменов нет.** Все соединения наружу исходящие: Telegram, Binance, R2, watchdog.
- **Желаемое состояние хранится в базе, команды дублируются по HTTP.** `strategies.desired_state` и `risk_state.locked_until`
  runner читает перед каждым входом, поэтому пауза и блокировка переживают рестарт и сетевые сбои.
- **Risk guard живёт в отдельном процессе** от стратегий, которые он ограничивает.
- **Стратегии работают плагинами в одном runner'е**, а не отдельным сервисом на каждую. Так укладываемся в бюджет.
- **Стоп каждой позиции стоит на бирже.** Падение любого сервиса не оставляет позицию без защиты.
- **Каждый ордер тегируется**: `clientOrderId = <префикс стратегии>-<ts>`. По тегу fill относится к стратегии,
  а всё без тега считается ручной торговлей (`manual`).

## Структура репозитория (план)

```
control/                 Go-модуль (Root Directory сервиса control)
  migrations/            пронумерованные SQL, применяются при старте control
  Dockerfile             с postgresql-client для /backup
  railway.json
runner/                  Python-пакет (Root Directory сервиса runner)
  strategies/            плагин на каждую стратегию: bounce, breakout, …
  api.py                 control API
  Dockerfile | railway.json
backup/                  cron-задача (Root Directory сервиса backup)
  Dockerfile | railway.json
docs/                    architecture.html, заметки
.github/workflows/ci.yml go test + pytest; Railway ждёт CI перед деплоем
```

> Когда у сервиса задан Root Directory, Railway собирает **только его папку**. Поэтому общий код между сервисами
> не переиспользуется, а миграции лежат внутри `control/`, а не в корне.

## Telegram-команды

| Команда | Действие |
|---|---|
| `/status` | счёт, PnL за день, просадка, позиции, блокировка |
| `/bots` | все стратегии: статус, позиции, PnL за сегодня, время последнего ответа; кнопки ⏸ ▶️ 📊 |
| `/bot <id>` | карточка стратегии; пауза / запуск / flatten (с подтверждением) / отчёт |
| `/pause`, `/resume <id\|all>` | остановить или вернуть новые входы; открытые позиции остаются под биржевыми стопами |
| `/report <id> [7d\|30d\|all]` | отчёт по стратегии + PNG с кривой equity |
| `/compare [период]` | сравнение стратегий и ручной торговли |
| `/limits`, `/lock` | лимиты (ослаблять — только вне блокировки и с задержкой 24 ч), ручная блокировка |
| `/backup`, `/backups` | свежий дамп базы или архив |

Доступ проверяется по числовому `user_id` (`ALLOWED_USER_IDS`), не по username. Каждая команда записывается в `events`.

## Control API runner'а

```
GET  /v1/health                     → runner, version, uptime, leader, exchange/ws ok
GET  /v1/strategies                 → [{id, state, desired, mode, open_positions, pnl_today, last_error}]
POST /v1/strategies/{id}/pause      {"reason","by"}
POST /v1/strategies/{id}/resume     {"by"}
POST /v1/strategies/{id}/flatten    {"confirm": "<одноразовый токен>"}
Заголовок каждого запроса: X-Control-Token
```

Состояния стратегии: `running` · `paused` · `flattening` · `locked` · `stale` (нет ответа > 60 с) · `offline` (> 5 мин).

## Данные (Postgres)

`strategies` · `runner_status` · `orders` · `fills` · `trades` · `equity_snapshots` · `cash_flows` · `risk_state` · `events` · `backups`.
API-ключи в базе не хранятся.

## Бэкапы и локальное восстановление

Бэкап делается каждый день в 03:00 UTC, перед каждой миграцией и по `/backup`. Формат: `pg_dump -Fc --no-owner --no-acl`,
проверка через `pg_restore --list`, рядом файл с sha256.

```
docker run -d --name trading-pg -e POSTGRES_PASSWORD=pg -p 5432:5432 postgres:17
docker exec trading-pg createdb -U postgres trading
docker exec -i trading-pg pg_restore -U postgres -d trading --no-owner --clean --if-exists < trading_YYYY-MM-DD_0300.dump
```

Major-версия `pg_dump` и `pg_restore` должна быть не ниже версии сервера на Railway.

## Деплой: GitHub → Railway

1. Репозиторий **приватный**. В Railway: New Project → Deploy from GitHub repo, регион **EU West (Amsterdam)**.
   С адресов в США Binance отвечает 451.
2. Сервисы `control`, `runner`, `backup` создаются из одного репо. Для каждого задаются Root Directory, Watch Paths
   (`/control/**` и т. д.) и путь к `railway.json`. Postgres берётся из шаблона.
3. Variables (sealed): `TELEGRAM_BOT_TOKEN`, `ALLOWED_USER_IDS`, `BINANCE_API_KEY`, `BINANCE_API_SECRET`, `ENCRYPTION_KEY`,
   `CONTROL_TOKEN` (shared), `DATABASE_URL=${{Postgres.DATABASE_URL}}`,
   `RUNNERS=swingbot=http://runner.railway.internal:8080`, ключи R2/B2, `HEALTHCHECK_URL`.
4. Включить **Wait for CI**: деплой начинается только после зелёных тестов в GitHub Actions.
5. В `main` попадает только проверенное. Откатиться можно через Redeploy прошлого деплоя в Railway.

Обязательно на Railway:

- **Replicas = 1** и leader-lock (`pg_try_advisory_lock`) в runner и control: во время деплоя две копии не торгуют
  и не опрашивают Telegram одновременно. Задать `RAILWAY_DEPLOYMENT_OVERLAP_SECONDS=0`.
- Обработка SIGTERM; `RAILWAY_DEPLOYMENT_DRAINING_SECONDS≈30`.
- Python-сервер слушает `::`, потому что приватная сеть Railway работает по IPv6.
- Restart policy: On Failure. Healthcheck Railway проверяет только при деплое, живость отслеживают control и внешний watchdog.
- **Не включать** hard usage limit (он остановит сервисы) и App Sleeping.
- На Hobby нет статического IP, поэтому нужен **отдельный ключ Binance для Railway**: Futures trade, без withdraw и transfer.

Бюджет по оценке ≈ $3.8–5.1/мес: control ~40 MB, runner ~150 MB, Postgres 80–150 MB, backup ~1 мин/сутки.
Сверить с railway.com/pricing и вкладкой Usage после первой недели.

## Секреты — никогда в git

`.env`, `config.txt`, `*.key`, `state.json`, `*.db`, `*.log`, `data/`, свечи и датасеты, `*.exe`. Корневой `.gitignore`
добавляется до первого коммита. Если ключ хоть раз попал в репозиторий, его нужно перевыпустить.

## План переноса

- [ ] 0 · `.gitignore`, CI, приватный репо на GitHub, Railway-проект в EU West, Postgres, отдельный Binance-ключ
- [ ] 1 · `control` ← `lossguard`: state → Postgres + миграции, `ALLOWED_USER_IDS`, ключи из env, `/healthz`, watchdog, снимки equity, `cash_flows`
- [ ] 2 · `runner` ← `swingbot`: журнал → Postgres, `desired_state` вместо STOP-файла, проверка `risk_lock`, control API, leader-lock, теги `clientOrderId`; неделя в paper-режиме
- [ ] 3 · опрос runner'ов, `/bots`, pause / resume / flatten, импорт fills (`journal`), `/report`, `/compare`, дайджест 23:59
- [ ] 4 · сервис `backup`, `/backup`, `/backups`, первое тестовое восстановление

Открытые вопросы:

- Один счёт или отдельный sub-account для ботов?
- Блокировка закрывает и ручные позиции (как сейчас) или только позиции ботов?
- Порог просадки от пика и лимиты по каждой стратегии.
