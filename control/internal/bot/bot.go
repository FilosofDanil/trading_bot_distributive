// Package bot is the Telegram front end: key setup wizard, /status, language choice.
package bot

import (
	"context"
	"errors"
	"log"
	"regexp"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"control/internal/config"
	"control/internal/guard"
	"control/internal/i18n"
	"control/internal/runners"
	"control/internal/store"
)

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9]{20,128}$`)

type Bot struct {
	api         *tgbotapi.BotAPI
	st          *store.Store
	g           *guard.Guard
	allowedIDs  map[int64]bool  // numeric Telegram user ids
	allowed     map[string]bool // normalized usernames; both empty = everyone
	defaultLang string
	presetKey   string // keys from the server config; when set /start skips the key dialog
	presetSec   string
	rr          *runners.Registry // trading-bot runners; nil = none configured
}

type Options struct {
	AllowedUserIDs   map[int64]bool
	AllowedUsernames map[string]bool
	DefaultLang      string
	PresetKey        string
	PresetSecret     string
}

func New(api *tgbotapi.BotAPI, st *store.Store, o Options) *Bot {
	return &Bot{api: api, st: st, allowedIDs: o.AllowedUserIDs, allowed: o.AllowedUsernames,
		defaultLang: o.DefaultLang, presetKey: o.PresetKey, presetSec: o.PresetSecret}
}

func (b *Bot) isAllowed(from *tgbotapi.User) bool {
	if len(b.allowedIDs) == 0 && len(b.allowed) == 0 {
		return true
	}
	if from == nil {
		return false
	}
	return b.allowedIDs[from.ID] || b.allowed[config.NormalizeUsername(from.UserName)]
}

// SetGuard wires the monitor (the guard needs the bot as its notifier, so it is set after construction).
func (b *Bot) SetGuard(g *guard.Guard) { b.g = g }

// Notify implements guard.Notifier.
func (b *Bot) Notify(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	for attempt := 0; attempt < 3; attempt++ {
		_, err := b.api.Send(msg)
		if err == nil {
			return
		}
		var te *tgbotapi.Error
		if errors.As(err, &te) && te.RetryAfter > 0 {
			time.Sleep(time.Duration(te.RetryAfter) * time.Second)
			continue
		}
		log.Printf("chat %d: send: %v", chatID, err)
		time.Sleep(time.Second)
	}
}

func (b *Bot) send(chatID int64, lang, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	msg.ReplyMarkup = keyboard(lang)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("chat %d: send: %v", chatID, err)
	}
}

func keyboard(lang string) tgbotapi.ReplyKeyboardMarkup {
	kb := tgbotapi.NewReplyKeyboard(tgbotapi.NewKeyboardButtonRow(
		tgbotapi.NewKeyboardButton(i18n.T(lang, "btn_status")),
		tgbotapi.NewKeyboardButton(i18n.T(lang, "btn_help")),
		tgbotapi.NewKeyboardButton(i18n.T(lang, "btn_lang")),
	))
	kb.ResizeKeyboard = true
	return kb
}

// RegisterCommands publishes the command menu in both languages.
func (b *Bot) RegisterCommands() {
	menus := map[string][]tgbotapi.BotCommand{
		"": {
			{Command: "start", Description: "Connect API keys and start monitoring"},
			{Command: "status", Description: "Balance, daily loss, block state"},
			{Command: "language", Description: "English / Русский"},
			{Command: "stop", Description: "Stop monitoring and delete keys"},
			{Command: "help", Description: "Help"},
			{Command: "bots", Description: "Trading bots and their state"},
			{Command: "bot", Description: "One bot: details and controls"},
			{Command: "pause", Description: "Pause a bot (no new positions)"},
			{Command: "resume", Description: "Resume a bot"},
			{Command: "run", Description: "Run the daily cycle now"},
			{Command: "plan", Description: "Last plan of a bot"},
			{Command: "positions", Description: "Open positions of a bot"},
			{Command: "report", Description: "Performance report of a bot"},
			{Command: "events", Description: "Recent events of a bot"},
			{Command: "debug", Description: "Runner diagnostics"},
			{Command: "ping", Description: "Check all runners"},
		},
		i18n.RU: {
			{Command: "start", Description: "Подключить API-ключи и включить мониторинг"},
			{Command: "status", Description: "Баланс, дневной убыток, блокировка"},
			{Command: "language", Description: "English / Русский"},
			{Command: "stop", Description: "Остановить мониторинг и удалить ключи"},
			{Command: "help", Description: "Помощь"},
			{Command: "bots", Description: "Торговые боты и их состояние"},
			{Command: "bot", Description: "Один бот: детали и управление"},
			{Command: "pause", Description: "Поставить бота на паузу"},
			{Command: "resume", Description: "Возобновить бота"},
			{Command: "run", Description: "Запустить дневной цикл сейчас"},
			{Command: "plan", Description: "Последний план бота"},
			{Command: "positions", Description: "Открытые позиции бота"},
			{Command: "report", Description: "Отчёт по боту"},
			{Command: "events", Description: "Последние события бота"},
			{Command: "debug", Description: "Диагностика раннеров"},
			{Command: "ping", Description: "Проверить все раннеры"},
		},
	}
	for lang, cmds := range menus {
		cfg := tgbotapi.NewSetMyCommandsWithScopeAndLanguage(tgbotapi.NewBotCommandScopeDefault(), lang, cmds...)
		if _, err := b.api.Request(cfg); err != nil {
			log.Printf("set commands (%q): %v", lang, err)
		}
	}
}

// Run processes Telegram updates until ctx ends.
func (b *Bot) Run(ctx context.Context) {
	uc := tgbotapi.NewUpdate(0)
	uc.Timeout = 50
	updates := b.api.GetUpdatesChan(uc)
	for {
		select {
		case <-ctx.Done():
			b.api.StopReceivingUpdates()
			return
		case up := <-updates:
			b.handle(ctx, up)
		}
	}
}

func (b *Bot) handle(ctx context.Context, up tgbotapi.Update) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic handling update %d: %v", up.UpdateID, r)
		}
	}()
	switch {
	case up.CallbackQuery != nil:
		b.handleCallback(ctx, up.CallbackQuery)
	case up.Message != nil && up.Message.Chat.IsPrivate():
		b.handleMessage(ctx, up.Message)
	}
}

func (b *Bot) langFor(chatID int64, from *tgbotapi.User) string {
	if u, ok := b.st.Get(chatID); ok && u.Lang != "" {
		return u.Lang
	}
	code := ""
	if from != nil {
		code = from.LanguageCode
	}
	return i18n.Normalize(code, b.defaultLang)
}

func (b *Bot) handleMessage(ctx context.Context, m *tgbotapi.Message) {
	chatID := m.Chat.ID
	lang := b.langFor(chatID, m.From)
	if !b.isAllowed(m.From) {
		if m.From != nil {
			log.Printf("rejected message from @%s (id %d)", m.From.UserName, m.From.ID)
		}
		b.send(chatID, lang, i18n.T(lang, "not_allowed"))
		return
	}
	if u, ok := b.st.Get(chatID); !ok || u.Lang == "" {
		_, _ = b.st.Update(chatID, func(u *store.User) { u.Lang = lang })
	}

	if m.IsCommand() {
		switch m.Command() {
		case "start":
			b.cmdStart(ctx, chatID, lang)
		case "status":
			b.cmdStatus(chatID, lang)
		case "language", "lang":
			b.cmdLanguage(chatID, lang)
		case "stop":
			b.cmdStop(chatID, lang)
		case "cancel":
			b.cmdCancel(chatID, lang)
		case "help":
			b.send(chatID, lang, i18n.T(lang, "help"))
		case "bots", "bot", "pause", "resume", "run", "plan", "positions", "report", "events", "debug", "ping":
			b.handleRunnerCommand(ctx, m, lang)
		default:
			b.send(chatID, lang, i18n.T(lang, "unknown"))
		}
		return
	}

	text := strings.TrimSpace(m.Text)
	u, _ := b.st.Get(chatID)
	switch u.Step {
	case store.StepAwaitKey:
		b.receiveKey(chatID, lang, m.MessageID, text)
		return
	case store.StepAwaitSecret:
		b.receiveSecret(ctx, chatID, lang, m.MessageID, text)
		return
	}

	switch {
	case isButton(text, "btn_status"), strings.EqualFold(text, "status"), strings.EqualFold(text, "статус"):
		b.cmdStatus(chatID, lang)
	case isButton(text, "btn_help"):
		b.send(chatID, lang, i18n.T(lang, "help"))
	case isButton(text, "btn_lang"):
		b.cmdLanguage(chatID, lang)
	default:
		b.send(chatID, lang, i18n.T(lang, "unknown"))
	}
}

func isButton(text, key string) bool {
	return text == i18n.T(i18n.EN, key) || text == i18n.T(i18n.RU, key)
}

func (b *Bot) cmdStart(ctx context.Context, chatID int64, lang string) {
	u, _ := b.st.Get(chatID)
	if u.Blocked {
		b.send(chatID, lang, i18n.T(lang, "blocked_refuse", b.g.FmtTime(u.BlockedUntil)))
		return
	}
	r := b.g.Rules()
	intro := i18n.T(lang, "intro", r.AlertPct, r.BlockPct, guard.Human(r.BlockFor, lang))
	if b.presetKey != "" {
		_, _ = b.st.Update(chatID, func(u *store.User) {
			u.Step = store.StepNone
			u.PendingKey = ""
		})
		b.send(chatID, lang, intro+"\n\n"+i18n.T(lang, "using_preset_keys"))
		b.activate(ctx, chatID, lang, b.presetKey, b.presetSec)
		return
	}
	_, _ = b.st.Update(chatID, func(u *store.User) {
		u.Step = store.StepAwaitKey
		u.PendingKey = ""
	})
	b.send(chatID, lang, intro+"\n\n"+i18n.T(lang, "ask_key"))
}

func (b *Bot) cmdCancel(chatID int64, lang string) {
	_, _ = b.st.Update(chatID, func(u *store.User) {
		u.Step = store.StepNone
		u.PendingKey = ""
	})
	b.send(chatID, lang, i18n.T(lang, "cancelled"))
}

func (b *Bot) cmdStop(chatID int64, lang string) {
	u, err := b.g.Deactivate(chatID)
	if errors.Is(err, guard.ErrBlocked) {
		b.send(chatID, lang, i18n.T(lang, "blocked_refuse", b.g.FmtTime(u.BlockedUntil)))
		return
	}
	if err != nil {
		log.Printf("chat %d: stop: %v", chatID, err)
	}
	b.send(chatID, lang, i18n.T(lang, "stopped"))
}

// deleteMessage removes a message that contained key material.
func (b *Bot) deleteMessage(chatID int64, msgID int) bool {
	_, err := b.api.Request(tgbotapi.NewDeleteMessage(chatID, msgID))
	return err == nil
}

func (b *Bot) receiveKey(chatID int64, lang string, msgID int, text string) {
	deleted := b.deleteMessage(chatID, msgID)
	if !keyPattern.MatchString(text) {
		b.send(chatID, lang, i18n.T(lang, "bad_key_format"))
		return
	}
	enc, err := b.st.Encrypt(text)
	if err != nil {
		log.Printf("chat %d: encrypt: %v", chatID, err)
		return
	}
	_, _ = b.st.Update(chatID, func(u *store.User) {
		u.PendingKey = enc
		u.Step = store.StepAwaitSecret
	})
	reply := i18n.T(lang, "ask_secret")
	if deleted {
		reply = i18n.T(lang, "msg_deleted") + "\n\n" + reply
	}
	b.send(chatID, lang, reply)
}

func (b *Bot) receiveSecret(ctx context.Context, chatID int64, lang string, msgID int, text string) {
	deleted := b.deleteMessage(chatID, msgID)
	if !keyPattern.MatchString(text) {
		b.send(chatID, lang, i18n.T(lang, "bad_key_format"))
		return
	}
	u, _ := b.st.Get(chatID)
	key, err := b.st.Decrypt(u.PendingKey)
	if err != nil {
		b.cmdStart(ctx, chatID, lang)
		return
	}
	note := i18n.T(lang, "checking_keys")
	if deleted {
		note = i18n.T(lang, "msg_deleted") + "\n" + note
	}
	b.send(chatID, lang, note)
	b.activate(ctx, chatID, lang, key, text)
}

// activate validates the keys with Binance and reports the starting balance and limits.
func (b *Bot) activate(ctx context.Context, chatID int64, lang, key, secret string) {
	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := b.g.Activate(actx, chatID, key, secret)
	if err != nil {
		_, _ = b.st.Update(chatID, func(u *store.User) {
			u.Step = store.StepNone
			u.PendingKey = ""
		})
		b.send(chatID, lang, i18n.T(lang, "keys_invalid", guard.Escape(err.Error())))
		return
	}
	r := b.g.Rules()
	base := res.User.Baseline
	b.send(chatID, lang, i18n.T(lang, "activated",
		guard.Money(base),
		guard.Money(base*(1-r.AlertPct/100)), r.AlertPct,
		guard.Money(base*(1-r.BlockPct/100)), r.BlockPct))
	if res.NoFuturesPerm {
		b.send(chatID, lang, i18n.T(lang, "no_futures_perm"))
	}
}

func (b *Bot) cmdStatus(chatID int64, lang string) {
	b.send(chatID, lang, StatusText(b.st, b.g, chatID, lang, time.Now()))
}

// StatusText renders the saved state of the last cron check.
func StatusText(st *store.Store, g *guard.Guard, chatID int64, lang string, now time.Time) string {
	u, _ := st.Get(chatID)
	if !u.Active {
		return i18n.T(lang, "not_configured")
	}
	l := u.Last
	if l.CheckedAt.IsZero() {
		return i18n.T(lang, "status_pending")
	}
	r := g.Rules()

	var state string
	resetIn := u.PeriodStart.Add(r.Period).Sub(now)
	switch {
	case u.Blocked:
		state = i18n.T(lang, "state_blocked", g.FmtTime(u.BlockedUntil), guard.Human(u.BlockedUntil.Sub(now), lang))
		resetIn = u.BlockedUntil.Sub(now)
	case u.Alerted:
		state = i18n.T(lang, "state_alerted", g.FmtTime(u.AlertedAt))
	default:
		state = i18n.T(lang, "state_ok")
	}
	if l.Error != "" {
		state += "\n" + i18n.T(lang, "state_error", guard.Escape(l.Error))
	}
	return i18n.T(lang, "status",
		guard.Human(now.Sub(l.CheckedAt), lang),
		guard.Money(l.WalletBalance), guard.Signed(l.UnrealizedPnL), guard.Money(l.Equity),
		guard.Money(u.Baseline), guard.Signed(-l.LossAmount), guard.Pct(l.LossPct),
		l.OpenPositions, r.AlertPct, r.BlockPct, guard.Human(resetIn, lang), state)
}

func (b *Bot) cmdLanguage(chatID int64, lang string) {
	msg := tgbotapi.NewMessage(chatID, i18n.T(lang, "choose_lang"))
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🇬🇧 English", "lang:"+i18n.EN),
		tgbotapi.NewInlineKeyboardButtonData("🇷🇺 Русский", "lang:"+i18n.RU),
	))
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("chat %d: send: %v", chatID, err)
	}
}

func (b *Bot) handleCallback(ctx context.Context, q *tgbotapi.CallbackQuery) {
	_, _ = b.api.Request(tgbotapi.NewCallback(q.ID, ""))
	if q.Message == nil || !b.isAllowed(q.From) {
		return
	}
	if strings.HasPrefix(q.Data, "rb:") {
		b.handleRunnerCallback(ctx, q)
		return
	}
	if !strings.HasPrefix(q.Data, "lang:") {
		return
	}
	lang := i18n.Normalize(strings.TrimPrefix(q.Data, "lang:"), b.defaultLang)
	chatID := q.Message.Chat.ID
	_, _ = b.st.Update(chatID, func(u *store.User) { u.Lang = lang })
	_, _ = b.api.Request(tgbotapi.NewDeleteMessage(chatID, q.Message.MessageID))
	b.send(chatID, lang, i18n.T(lang, "lang_set"))
}
