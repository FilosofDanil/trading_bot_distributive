// Package i18n holds the English and Russian bot texts. Messages use Telegram HTML.
package i18n

import (
	"fmt"
	"strings"
)

const (
	EN = "en"
	RU = "ru"
)

// Normalize maps a Telegram language_code (or anything else) to a supported language.
func Normalize(code, def string) string {
	code = strings.ToLower(code)
	switch {
	case strings.HasPrefix(code, "ru"), strings.HasPrefix(code, "uk"), strings.HasPrefix(code, "be"):
		return RU
	case strings.HasPrefix(code, "en"):
		return EN
	}
	if def == RU {
		return RU
	}
	return EN
}

// T returns the message for key in lang, formatted with args.
func T(lang, key string, args ...any) string {
	m, ok := messages[key]
	if !ok {
		return key
	}
	s, ok := m[lang]
	if !ok {
		s = m[EN]
	}
	if len(args) == 0 {
		return s
	}
	return fmt.Sprintf(s, args...)
}

var messages = map[string]map[string]string{
	"intro": {
		EN: "👋 <b>LossGuard</b> watches your Binance futures account (Tiger.Trade keys work) every 10 seconds.\n\n" +
			"• loss ≥ <b>%.0f%%</b> of the day's starting balance (unrealized PnL included) → alert\n" +
			"• loss ≥ <b>%.0f%%</b> → all positions and orders are closed and trading is blocked for <b>%s</b>: " +
			"any new order is closed immediately",
		RU: "👋 <b>LossGuard</b> каждые 10 секунд проверяет ваш фьючерсный счёт Binance (ключи Tiger.Trade подходят).\n\n" +
			"• убыток ≥ <b>%.0f%%</b> от баланса на начало дня (с учётом нереализованного PnL) → предупреждение\n" +
			"• убыток ≥ <b>%.0f%%</b> → все позиции и ордера закрываются, торговля блокируется на <b>%s</b>: " +
			"любой новый ордер сразу закрывается",
	},
	"ask_key": {
		EN: "Create an API key with <b>Enable Futures</b> (reading + trading, no withdrawals). " +
			"Restricting it to your server IP is recommended.\n\nStep 1/2: send me the <b>API Key</b>.",
		RU: "Создайте API-ключ с правом <b>Enable Futures</b> (чтение + торговля, без вывода средств). " +
			"Рекомендуется ограничить ключ IP-адресом сервера.\n\nШаг 1/2: отправьте <b>API Key</b>.",
	},
	"using_preset_keys": {
		EN: "🔑 Using the account API keys from the server config.\n⏳ Checking the keys…",
		RU: "🔑 Использую API-ключи счёта из конфигурации сервера.\n⏳ Проверяю ключи…",
	},
	"ask_secret": {
		EN: "Step 2/2: now send the <b>Secret Key</b>.",
		RU: "Шаг 2/2: теперь отправьте <b>Secret Key</b>.",
	},
	"bad_key_format": {
		EN: "That does not look like a Binance key (expected one line of 20–128 letters/digits). Try again.",
		RU: "Это не похоже на ключ Binance (ожидается одна строка из 20–128 букв/цифр). Попробуйте ещё раз.",
	},
	"msg_deleted": {
		EN: "🔒 I deleted your message with the key from the chat.",
		RU: "🔒 Сообщение с ключом удалено из чата.",
	},
	"checking_keys": {
		EN: "⏳ Checking the keys…",
		RU: "⏳ Проверяю ключи…",
	},
	"keys_invalid": {
		EN: "❌ Binance rejected the keys: <code>%s</code>\nSend /start to try again.",
		RU: "❌ Binance отклонил ключи: <code>%s</code>\nОтправьте /start, чтобы попробовать снова.",
	},
	"no_futures_perm": {
		EN: "⚠️ This key does not have <b>Enable Futures</b>. Without it I cannot close positions when the block triggers. Edit the key on Binance — I keep monitoring meanwhile.",
		RU: "⚠️ У ключа не включено <b>Enable Futures</b>. Без этого я не смогу закрыть позиции при блокировке. Измените ключ на Binance — мониторинг пока продолжается.",
	},
	"activated": {
		EN: "✅ Monitoring is on.\nStarting balance for the next 24h: <b>%s</b> USDT\nAlert at <b>%s</b> USDT (−%.0f%%), block at <b>%s</b> USDT (−%.0f%%).\nUse /status any time.",
		RU: "✅ Мониторинг включён.\nСтартовый баланс на ближайшие 24 ч: <b>%s</b> USDT\nПредупреждение при <b>%s</b> USDT (−%.0f%%), блокировка при <b>%s</b> USDT (−%.0f%%).\nКоманда /status — в любой момент.",
	},
	"status": {
		EN: "📊 <b>Status</b> (checked %s ago)\n\n" +
			"Wallet balance: <b>%s</b> USDT\nUnrealized PnL: <b>%s</b> USDT\nEquity: <b>%s</b> USDT\n" +
			"Day start balance: <b>%s</b> USDT\nDay result: <b>%s</b> USDT (<b>%s%%</b>)\n" +
			"Open positions: <b>%d</b>\n\nAlert at −%.0f%% · Block at −%.0f%%\nWindow resets in %s\n%s",
		RU: "📊 <b>Статус</b> (проверка %s назад)\n\n" +
			"Баланс кошелька: <b>%s</b> USDT\nНереализованный PnL: <b>%s</b> USDT\nЭквити: <b>%s</b> USDT\n" +
			"Баланс на начало дня: <b>%s</b> USDT\nРезультат дня: <b>%s</b> USDT (<b>%s%%</b>)\n" +
			"Открытых позиций: <b>%d</b>\n\nПредупреждение при −%.0f%% · Блокировка при −%.0f%%\nСброс окна через %s\n%s",
	},
	"state_ok": {
		EN: "🟢 Trading allowed",
		RU: "🟢 Торговля разрешена",
	},
	"state_alerted": {
		EN: "🟡 Alert sent at %s",
		RU: "🟡 Предупреждение отправлено в %s",
	},
	"state_blocked": {
		EN: "🔴 TRADING BLOCKED until %s (%s left)",
		RU: "🔴 ТОРГОВЛЯ ЗАБЛОКИРОВАНА до %s (осталось %s)",
	},
	"state_error": {
		EN: "⚠️ Last check failed: <code>%s</code>",
		RU: "⚠️ Последняя проверка не удалась: <code>%s</code>",
	},
	"status_pending": {
		EN: "⏳ Monitoring is on, the first check has not finished yet. Try again in a few seconds.",
		RU: "⏳ Мониторинг включён, первая проверка ещё не завершена. Повторите через несколько секунд.",
	},
	"not_configured": {
		EN: "Monitoring is off. Send /start to connect your API keys.",
		RU: "Мониторинг выключен. Отправьте /start, чтобы подключить API-ключи.",
	},
	"alert": {
		EN: "⚠️ <b>Daily loss alert</b>\n\nYou are down <b>%s USDT (%s%%)</b> from the day's starting balance %s USDT.\nEquity: %s USDT (unrealized PnL %s).\n\nAt −%.0f%% (%s USDT) all positions will be closed and trading blocked for %s.",
		RU: "⚠️ <b>Предупреждение о дневном убытке</b>\n\nУбыток <b>%s USDT (%s%%)</b> от баланса на начало дня %s USDT.\nЭквити: %s USDT (нереализованный PnL %s).\n\nПри −%.0f%% (%s USDT) все позиции будут закрыты, а торговля заблокирована на %s.",
	},
	"blocked": {
		EN: "🛑 <b>Trading blocked</b>\n\nDaily loss reached <b>%s USDT (%s%%)</b>.\nClosed positions: %d · cancelled orders on %d symbol(s).\n\nBlocked until <b>%s</b>. Any order you place before then is closed automatically.",
		RU: "🛑 <b>Торговля заблокирована</b>\n\nДневной убыток достиг <b>%s USDT (%s%%)</b>.\nЗакрыто позиций: %d · отменены ордера по %d символ(ам).\n\nБлокировка до <b>%s</b>. Любой ордер до этого времени будет автоматически закрыт.",
	},
	"enforced": {
		EN: "🛑 Trading is blocked until %s. Closed %d position(s), cancelled orders on %d symbol(s).",
		RU: "🛑 Торговля заблокирована до %s. Закрыто позиций: %d, отменены ордера по %d символ(ам).",
	},
	"enforce_errors": {
		EN: "❗ Some actions failed — check the terminal manually:\n<code>%s</code>",
		RU: "❗ Часть действий не выполнилась — проверьте терминал вручную:\n<code>%s</code>",
	},
	"unblocked": {
		EN: "✅ The 24h block is over. New day starting balance: <b>%s</b> USDT.",
		RU: "✅ 24-часовая блокировка закончилась. Новый стартовый баланс дня: <b>%s</b> USDT.",
	},
	"check_failing": {
		EN: "⚠️ I cannot read your account: <code>%s</code>\nI keep retrying every 10 seconds.",
		RU: "⚠️ Не удаётся прочитать счёт: <code>%s</code>\nПовторяю попытки каждые 10 секунд.",
	},
	"check_recovered": {
		EN: "✅ Account access restored.",
		RU: "✅ Доступ к счёту восстановлен.",
	},
	"blocked_refuse": {
		EN: "🔴 Trading is blocked until %s. Keys and monitoring cannot be changed until the block ends.",
		RU: "🔴 Торговля заблокирована до %s. Ключи и мониторинг нельзя менять до окончания блокировки.",
	},
	"stopped": {
		EN: "Monitoring stopped and your keys were deleted. Send /start to connect again.",
		RU: "Мониторинг остановлен, ключи удалены. Отправьте /start, чтобы подключиться снова.",
	},
	"cancelled": {
		EN: "Setup cancelled.",
		RU: "Настройка отменена.",
	},
	"choose_lang": {
		EN: "Choose the language:",
		RU: "Выберите язык:",
	},
	"lang_set": {
		EN: "Language: English 🇬🇧",
		RU: "Язык: русский 🇷🇺",
	},
	"help": {
		EN: "<b>Commands</b>\n/start — connect API keys and start monitoring\n/status — balance, daily loss and block state\n/language — English / Русский\n/stop — stop monitoring and delete keys (not while blocked)\n/cancel — cancel key setup\n/help — this message\n\n" +
			"<b>Trading bots</b>\n/bots — all bots: state, equity, PnL (with buttons)\n/bot &lt;id&gt; — one bot: details and controls\n" +
			"/pause &lt;id&gt; · /resume &lt;id&gt; — stop / allow new positions\n/run &lt;id&gt; — run the daily cycle now\n" +
			"/plan &lt;id&gt; — last plan\n/positions &lt;id&gt; — open positions\n/report &lt;id&gt; — performance report\n" +
			"/events &lt;id&gt; [n] — recent events\n/debug [runner] — runner diagnostics\n/ping — check all runners\n" +
			"&lt;id&gt; may be shortened to a unique prefix or the runner name.",
		RU: "<b>Команды</b>\n/start — подключить API-ключи и включить мониторинг\n/status — баланс, дневной убыток и состояние блокировки\n/language — English / Русский\n/stop — остановить мониторинг и удалить ключи (не во время блокировки)\n/cancel — отменить ввод ключей\n/help — это сообщение\n\n" +
			"<b>Торговые боты</b>\n/bots — все боты: состояние, эквити, PnL (с кнопками)\n/bot &lt;id&gt; — один бот: детали и управление\n" +
			"/pause &lt;id&gt; · /resume &lt;id&gt; — запретить / разрешить новые позиции\n/run &lt;id&gt; — запустить дневной цикл сейчас\n" +
			"/plan &lt;id&gt; — последний план\n/positions &lt;id&gt; — открытые позиции\n/report &lt;id&gt; — отчёт о результатах\n" +
			"/events &lt;id&gt; [n] — последние события\n/debug [раннер] — диагностика раннеров\n/ping — проверить все раннеры\n" +
			"&lt;id&gt; можно сократить до уникального префикса или имени раннера.",
	},
	"not_allowed": {
		EN: "This bot is private.",
		RU: "Это приватный бот.",
	},
	"unknown": {
		EN: "I did not understand that. /help lists the commands.",
		RU: "Не понял команду. Список команд — /help.",
	},
	"btn_status": {
		EN: "📊 Status",
		RU: "📊 Статус",
	},
	"btn_help": {
		EN: "❓ Help",
		RU: "❓ Помощь",
	},
	"btn_lang": {
		EN: "🌐 Language",
		RU: "🌐 Язык",
	},
	"btn_bots": {
		EN: "🤖 Bots",
		RU: "🤖 Боты",
	},
	"btn_menu": {
		EN: "⬅️ Menu",
		RU: "⬅️ Меню",
	},
	"menu_shown": {
		EN: "Main menu — use the buttons below.",
		RU: "Главное меню — кнопки внизу.",
	},
	"bots_pick": {
		EN: "Tap a bot below to open its controls.",
		RU: "Нажмите на бота внизу, чтобы открыть управление.",
	},

	// ---- trading-bot runners ----
	"runners_none": {
		EN: "No trading bots are configured (the RUNNERS variable is empty).",
		RU: "Торговые боты не настроены (переменная RUNNERS пуста).",
	},
	"bots_title": {
		EN: "🤖 <b>Trading bots</b>",
		RU: "🤖 <b>Торговые боты</b>",
	},
	"bots_line": {
		EN: "%d. %s <b>%s</b> · %s · %s\n      positions: %d · equity %s · PnL %s · polled %s ago",
		RU: "%d. %s <b>%s</b> · %s · %s\n      позиций: %d · эквити %s · PnL %s · опрос %s назад",
	},
	"bots_runner_only": {
		EN: "%s runner <b>%s</b> · %s · <code>%s</code>",
		RU: "%s раннер <b>%s</b> · %s · <code>%s</code>",
	},
	"bot_card": {
		EN: "%s <b>%s</b> — %s\nMode: <b>%s</b> · state: <b>%s</b> · desired: <b>%s</b>\nOpen positions: <b>%d</b>\nBook: %s\n" +
			"Capital: %s USDT → equity <b>%s</b> USDT\nPnL total: <b>%s</b> · today: <b>%s</b>\nLast run: %s (%s)\nNext run: %s\n" +
			"Last error: %s\nRunner: <b>%s</b> · %s · polled %s ago",
		RU: "%s <b>%s</b> — %s\nРежим: <b>%s</b> · состояние: <b>%s</b> · задано: <b>%s</b>\nОткрытых позиций: <b>%d</b>\nКнига: %s\n" +
			"Капитал: %s USDT → эквити <b>%s</b> USDT\nPnL всего: <b>%s</b> · сегодня: <b>%s</b>\nПоследний запуск: %s (%s)\nСледующий запуск: %s\n" +
			"Последняя ошибка: %s\nРаннер: <b>%s</b> · %s · опрос %s назад",
	},
	"bot_health": {
		EN: "Health: v%s · uptime %s · Binance %s (%s ms) · busy: %s",
		RU: "Здоровье: v%s · аптайм %s · Binance %s (%s мс) · занят: %s",
	},
	"rs_running": {EN: "running", RU: "запущен"},
	"rs_paused":  {EN: "paused", RU: "на паузе"},
	"rs_busy":    {EN: "busy", RU: "занят"},
	"rs_error":   {EN: "error", RU: "ошибка"},
	"rs_ok":      {EN: "runner reachable", RU: "раннер доступен"},
	"rs_stale":   {EN: "runner stale", RU: "раннер не отвечает"},
	"rs_unreachable": {
		EN: "runner unreachable",
		RU: "раннер недоступен",
	},
	"mode_paper": {EN: "PAPER", RU: "БУМАЖНЫЙ"},
	"mode_live":  {EN: "LIVE", RU: "БОЕВОЙ"},
	"yes":        {EN: "yes", RU: "да"},
	"no":         {EN: "no", RU: "нет"},
	"rb_pause":   {EN: "⏸ Pause", RU: "⏸ Пауза"},
	"rb_resume":  {EN: "▶️ Resume", RU: "▶️ Возобновить"},
	"rb_run":     {EN: "🚀 Run now", RU: "🚀 Запустить"},
	"rb_plan":    {EN: "📋 Plan", RU: "📋 План"},
	"rb_pos":     {EN: "💼 Positions", RU: "💼 Позиции"},
	"rb_report":  {EN: "📊 Report", RU: "📊 Отчёт"},
	"rr_paused": {
		EN: "⏸ <b>%s</b> is paused (state: %s). No new positions; held ones are still managed.",
		RU: "⏸ <b>%s</b> поставлен на паузу (состояние: %s). Новых позиций не будет, открытые сопровождаются.",
	},
	"rr_resumed": {
		EN: "▶️ <b>%s</b> resumed (state: %s).",
		RU: "▶️ <b>%s</b> возобновлён (состояние: %s).",
	},
	"rr_run_started": {
		EN: "🚀 <b>%s</b>: the daily cycle has started.",
		RU: "🚀 <b>%s</b>: дневной цикл запущен.",
	},
	"rr_run_preview": {
		EN: "🔍 <b>%s</b>: today is already accepted, computing a preview plan only. It takes up to a few minutes; the result arrives as a message (then 📋 Plan shows it).",
		RU: "🔍 <b>%s</b>: сегодняшний день уже принят, считается только предварительный план. Это занимает до нескольких минут; результат придёт сообщением (потом его покажет 📋 План).",
	},
	"rr_busy": {
		EN: "⏳ <b>%s</b> is busy: <code>%s</code>",
		RU: "⏳ <b>%s</b> занят: <code>%s</code>",
	},
	"rr_not_found": {
		EN: "<b>%s</b>: nothing there yet (<code>%s</code>).",
		RU: "<b>%s</b>: данных пока нет (<code>%s</code>).",
	},
	"rr_error": {
		EN: "❌ <code>%s</code>",
		RU: "❌ <code>%s</code>",
	},
	"plan_head": {
		EN: "📋 <b>%s</b> — plan for <b>%s</b>\nAccepted: %s · preview: %s · paused: %s\nEquity: %s USDT · idle capital: %s USDT\nBook: %s",
		RU: "📋 <b>%s</b> — план на <b>%s</b>\nПринят: %s · предварительный: %s · пауза: %s\nЭквити: %s USDT · свободный капитал: %s USDT\nКнига: %s",
	},
	"plan_lists": {
		EN: "Open: %s\nClose: %s\nResize: %s\nPair stopped: %s\nRehedge: %s\nTop funding (%% per 8h): %s\nMin funding: %s%% per 8h",
		RU: "Открыть: %s\nЗакрыть: %s\nИзменить размер: %s\nПара остановлена: %s\nПерехеджировать: %s\nТоп фандинга (%% за 8ч): %s\nМин. фандинг: %s%% за 8ч",
	},
	"plan_paused": {
		EN: "⏸ The bot is paused: no new pairs are opened, held ones are still managed. Without the pause it would open: %s. Press ▶️ Resume to trade.",
		RU: "⏸ Бот на паузе: новые пары не открываются, открытые сопровождаются. Без паузы открыл бы: %s. Нажмите ▶️ Возобновить, чтобы торговать.",
	},
	"plan_below_min": {
		EN: "💤 No coin reaches the minimum funding of %s%% per 8h, so the book is empty. This is normal: the bot waits and checks again every day.",
		RU: "💤 Ни одна монета не дотягивает до минимального фандинга %s%% за 8ч, поэтому книга пустая. Это нормально: бот ждёт и проверяет снова каждый день.",
	},
	"rr_run_paused": {
		EN: "⏸ <b>%s</b> is paused, so the plan opens no new pairs; it shows what a resume would open. Press ▶️ Resume to trade.",
		RU: "⏸ <b>%s</b> на паузе, поэтому план не откроет новых пар; он покажет, что открылось бы после возобновления. Нажмите ▶️ Возобновить, чтобы торговать.",
	},
	"plan_orders_title": {EN: "Orders:", RU: "Ордера:"},
	"hdr_orders":        {EN: "symbol|spot|perp|notional|ok", RU: "символ|спот|перп|объём|ок"},
	"hdr_positions":     {EN: "symbol|n|days|funding|basis|fees|pnl", RU: "символ|n|дни|фандинг|базис|комис.|pnl"},
	"plan_skipped": {
		EN: "Skipped (%d):",
		RU: "Пропущено (%d):",
	},
	"pos_head": {
		EN: "💼 <b>%s</b> — positions (%d)\n",
		RU: "💼 <b>%s</b> — позиции (%d)\n",
	},
	"pos_total": {
		EN: "Total PnL: <b>%s</b> USDT",
		RU: "Итого PnL: <b>%s</b> USDT",
	},
	"pos_none": {
		EN: "<b>%s</b>: no open positions.",
		RU: "<b>%s</b>: открытых позиций нет.",
	},
	"report": {
		EN: "📊 <b>%s</b> — report\nStart: %s · days: %d (active: %d)\nCapital: %s → equity <b>%s</b> USDT\n" +
			"Return: <b>%s%%</b> · annual: %s%% · max drawdown: %s%%\nFunding: %s · basis: %s · fees: %s USDT\n" +
			"Pairs: opened %d · closed %d · stops %d\n<code>%s</code>",
		RU: "📊 <b>%s</b> — отчёт\nСтарт: %s · дней: %d (активных: %d)\nКапитал: %s → эквити <b>%s</b> USDT\n" +
			"Доходность: <b>%s%%</b> · годовых: %s%% · макс. просадка: %s%%\nФандинг: %s · базис: %s · комиссии: %s USDT\n" +
			"Пары: открыто %d · закрыто %d · стопов %d\n<code>%s</code>",
	},
	"ev_head": {
		EN: "🧾 <b>%s</b> — last %d events (UTC)",
		RU: "🧾 <b>%s</b> — последние события: %d (UTC)",
	},
	"ev_none": {
		EN: "<b>%s</b>: no events yet.",
		RU: "<b>%s</b>: событий пока нет.",
	},
	"dbg_runner": {
		EN: "🛠 <b>%s</b>\nURL: <code>%s</code>\nOK: %s · latency: %d ms · fail streak: %d\nLast OK: %s ago\nLast error: %s",
		RU: "🛠 <b>%s</b>\nURL: <code>%s</code>\nOK: %s · задержка: %d мс · подряд сбоев: %d\nПоследний успех: %s назад\nПоследняя ошибка: %s",
	},
	"dbg_health": {
		EN: "Version: %s · uptime: %s · mode: %s\nBinance: %s (%s ms)\nRSS: %s MB · universe days: %d · state: %d bytes · busy: %s",
		RU: "Версия: %s · аптайм: %s · режим: %s\nBinance: %s (%s мс)\nRSS: %s МБ · дней в универсуме: %d · состояние: %d байт · занят: %s",
	},
	"dbg_errors":   {EN: "Last errors:", RU: "Последние ошибки:"},
	"dbg_nohealth": {EN: "No health data yet.", RU: "Данных о здоровье пока нет."},
	"ping_line": {
		EN: "%s <b>%s</b> · %d ms",
		RU: "%s <b>%s</b> · %d мс",
	},
	"ping_fail": {
		EN: "%s <b>%s</b> · down (%d failed) · <code>%s</code>",
		RU: "%s <b>%s</b> · не отвечает (сбоев: %d) · <code>%s</code>",
	},
	"alert_runner_down": {
		EN: "🔴 Runner <b>%s</b> is unreachable (%d failed polls): <code>%s</code>",
		RU: "🔴 Раннер <b>%s</b> недоступен (сбоев опроса подряд: %d): <code>%s</code>",
	},
	"alert_runner_up": {
		EN: "🟢 Runner <b>%s</b> is reachable again.",
		RU: "🟢 Раннер <b>%s</b> снова доступен.",
	},
	"alert_strategy_error": {
		EN: "⚠️ <b>%s</b> reports an error: <code>%s</code>",
		RU: "⚠️ <b>%s</b> сообщает об ошибке: <code>%s</code>",
	},
	"alert_event": {
		EN: "%s <b>%s</b>: %s",
		RU: "%s <b>%s</b>: %s",
	},
}

// Keys exposes the message keys (used by tests to check both languages are complete).
func Keys() []string {
	out := make([]string, 0, len(messages))
	for k := range messages {
		out = append(out, k)
	}
	return out
}

// Has reports whether key has a text for lang.
func Has(lang, key string) bool {
	_, ok := messages[key][lang]
	return ok
}
