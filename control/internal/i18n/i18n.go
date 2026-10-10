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
		EN: "<b>Commands</b>\n/start — connect API keys and start monitoring\n/status — balance, daily loss and block state\n/language — English / Русский\n/stop — stop monitoring and delete keys (not while blocked)\n/cancel — cancel key setup\n/help — this message",
		RU: "<b>Команды</b>\n/start — подключить API-ключи и включить мониторинг\n/status — баланс, дневной убыток и состояние блокировки\n/language — English / Русский\n/stop — остановить мониторинг и удалить ключи (не во время блокировки)\n/cancel — отменить ввод ключей\n/help — это сообщение",
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
