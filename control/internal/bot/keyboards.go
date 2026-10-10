package bot

import (
	"context"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"control/internal/i18n"
	"control/internal/store"
)

// All buttons are reply (text) keyboards: a press arrives as an ordinary message with the button
// text, which handleButton maps back to an action. The keyboard shown with each reply follows the
// chat's current view (main menu, bot list, one bot's controls, language choice).

const (
	viewMain  = ""
	viewBots  = "bots"
	viewLang  = "lang"
	viewStrat = "s:" // + strategy id

	botPrefix = "🤖 "  // bot list button: "🤖 <strategy id>"
	idSep     = " · " // strategy control button: "<label> · <strategy id>"

	langEN = "🇬🇧 English"
	langRU = "🇷🇺 Русский"
)

// strategyActions maps a control-button label key to the runner action it triggers, in display order.
var strategyActions = []struct{ key, action string }{
	{"rb_pause", "pause"},
	{"rb_resume", "resume"},
	{"rb_run", "run"},
	{"rb_plan", "plan"},
	{"rb_pos", "positions"},
	{"rb_report", "report"},
}

func (b *Bot) setView(chatID int64, v string) {
	b.viewMu.Lock()
	defer b.viewMu.Unlock()
	if b.views == nil {
		b.views = map[int64]string{}
	}
	if v == viewMain {
		delete(b.views, chatID)
		return
	}
	b.views[chatID] = v
}

func (b *Bot) view(chatID int64) string {
	b.viewMu.Lock()
	defer b.viewMu.Unlock()
	return b.views[chatID]
}

// keyboardFor renders the keyboard of the chat's current view.
func (b *Bot) keyboardFor(chatID int64, lang string) tgbotapi.ReplyKeyboardMarkup {
	v := b.view(chatID)
	switch {
	case v == viewBots:
		return b.botsKeyboard(lang)
	case v == viewLang:
		return langKeyboard(lang)
	case strings.HasPrefix(v, viewStrat):
		return strategyKeyboard(lang, strings.TrimPrefix(v, viewStrat))
	}
	return mainKeyboard(lang)
}

func replyKeyboard(rows ...[]tgbotapi.KeyboardButton) tgbotapi.ReplyKeyboardMarkup {
	kb := tgbotapi.NewReplyKeyboard(rows...)
	kb.ResizeKeyboard = true
	return kb
}

func buttons(texts ...string) []tgbotapi.KeyboardButton {
	row := make([]tgbotapi.KeyboardButton, len(texts))
	for i, t := range texts {
		row[i] = tgbotapi.NewKeyboardButton(t)
	}
	return row
}

func menuRow(lang string) []tgbotapi.KeyboardButton {
	return buttons(i18n.T(lang, "btn_menu"))
}

func mainKeyboard(lang string) tgbotapi.ReplyKeyboardMarkup {
	return replyKeyboard(
		buttons(i18n.T(lang, "btn_status"), i18n.T(lang, "btn_bots")),
		buttons(i18n.T(lang, "btn_help"), i18n.T(lang, "btn_lang")),
	)
}

func langKeyboard(lang string) tgbotapi.ReplyKeyboardMarkup {
	return replyKeyboard(buttons(langEN, langRU), menuRow(lang))
}

// botsKeyboard has one "🤖 <id>" button per known strategy, two per row.
func (b *Bot) botsKeyboard(lang string) tgbotapi.ReplyKeyboardMarkup {
	var rows [][]tgbotapi.KeyboardButton
	var cur []tgbotapi.KeyboardButton
	if !b.rr.Empty() {
		for _, r := range b.allStrategies() {
			cur = append(cur, tgbotapi.NewKeyboardButton(botPrefix+r.s.ID))
			if len(cur) == 2 {
				rows, cur = append(rows, cur), nil
			}
		}
	}
	if len(cur) > 0 {
		rows = append(rows, cur)
	}
	return replyKeyboard(append(rows, buttons(i18n.T(lang, "btn_bots"), i18n.T(lang, "btn_menu")))...)
}

func strategyButton(lang, key, id string) string {
	return i18n.T(lang, key) + idSep + id
}

func strategyKeyboard(lang, id string) tgbotapi.ReplyKeyboardMarkup {
	return replyKeyboard(
		buttons(strategyButton(lang, "rb_pause", id), strategyButton(lang, "rb_resume", id)),
		buttons(strategyButton(lang, "rb_run", id), strategyButton(lang, "rb_plan", id)),
		buttons(strategyButton(lang, "rb_pos", id), strategyButton(lang, "rb_report", id)),
		buttons(i18n.T(lang, "btn_bots"), i18n.T(lang, "btn_menu")),
	)
}

// parseStrategyButton maps "<label> · <id>" (either language) back to a runner action.
func parseStrategyButton(text string) (action, id string, ok bool) {
	for _, a := range strategyActions {
		for _, lang := range []string{i18n.EN, i18n.RU} {
			if rest, found := strings.CutPrefix(text, i18n.T(lang, a.key)+idSep); found && strings.TrimSpace(rest) != "" {
				return a.action, strings.TrimSpace(rest), true
			}
		}
	}
	return "", "", false
}

// handleButton serves a keyboard press; false = the text is not a button.
func (b *Bot) handleButton(ctx context.Context, m *tgbotapi.Message, lang, text string) bool {
	chatID := m.Chat.ID
	var by int64
	if m.From != nil {
		by = m.From.ID
	}
	switch {
	case isButton(text, "btn_status"):
		b.setView(chatID, viewMain)
		b.cmdStatus(chatID, lang)
	case isButton(text, "btn_help"):
		b.setView(chatID, viewMain)
		b.send(chatID, lang, i18n.T(lang, "help"))
	case isButton(text, "btn_menu"):
		b.setView(chatID, viewMain)
		b.send(chatID, lang, i18n.T(lang, "menu_shown"))
	case isButton(text, "btn_lang"):
		b.cmdLanguage(chatID, lang)
	case isButton(text, "btn_bots"):
		b.runnerAction(ctx, chatID, lang, by, "bots", "")
	case text == langEN:
		b.setLanguage(chatID, i18n.EN)
	case text == langRU:
		b.setLanguage(chatID, i18n.RU)
	default:
		if id, found := strings.CutPrefix(text, botPrefix); found && strings.TrimSpace(id) != "" {
			b.runnerAction(ctx, chatID, lang, by, "bot", strings.TrimSpace(id))
			return true
		}
		action, id, ok := parseStrategyButton(text)
		if !ok {
			return false
		}
		b.runnerAction(ctx, chatID, lang, by, action, id)
	}
	return true
}

func (b *Bot) cmdLanguage(chatID int64, lang string) {
	b.setView(chatID, viewLang)
	b.send(chatID, lang, i18n.T(lang, "choose_lang"))
}

func (b *Bot) setLanguage(chatID int64, lang string) {
	_, _ = b.st.Update(chatID, func(u *store.User) { u.Lang = lang })
	b.setView(chatID, viewMain)
	b.send(chatID, lang, i18n.T(lang, "lang_set"))
}
