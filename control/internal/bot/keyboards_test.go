package bot

import (
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"control/internal/i18n"
)

func texts(kb tgbotapi.ReplyKeyboardMarkup) []string {
	var out []string
	for _, row := range kb.Keyboard {
		for _, b := range row {
			out = append(out, b.Text)
		}
	}
	return out
}

func TestStrategyButtonsRoundTrip(t *testing.T) {
	for _, lang := range []string{i18n.EN, i18n.RU} {
		for _, id := range []string{"funding", "funding.carry", "b_2"} {
			seen := map[string]bool{}
			for _, text := range texts(strategyKeyboard(lang, id)) {
				if isButton(text, "btn_bots") || isButton(text, "btn_menu") {
					continue
				}
				action, got, ok := parseStrategyButton(text)
				if !ok || got != id {
					t.Fatalf("%s: %q -> %q %q %v", lang, text, action, got, ok)
				}
				seen[action] = true
			}
			if len(seen) != len(strategyActions)-1 || seen["flatten_yes"] { // the confirmation has its own keyboard
				t.Errorf("%s/%s: actions %v", lang, id, seen)
			}
		}
	}
}

func TestFlattenNeedsAFreshConfirmationForTheSameBot(t *testing.T) {
	b := &Bot{}
	const chat = 5
	b.askFlatten(chat, "funding.carry")
	kb := texts(b.keyboardFor(chat, i18n.RU))
	if a, id, ok := parseStrategyButton(kb[0]); !ok || a != "flatten_yes" || id != "funding.carry" {
		t.Fatalf("confirmation keyboard: %v", kb)
	}
	if b.takeFlatten(chat, "other") {
		t.Fatal("a confirmation for another bot must not count")
	}
	if b.takeFlatten(chat, "funding.carry") {
		t.Fatal("a request is consumed by the first answer")
	}
	b.askFlatten(chat, "funding.carry")
	b.flatten[chat] = flattenAsk{"funding.carry", time.Now().Add(-flattenTTL - time.Second)}
	if b.takeFlatten(chat, "funding.carry") {
		t.Fatal("an expired request must not count")
	}
	b.askFlatten(chat, "funding.carry")
	if !b.takeFlatten(chat, "funding.carry") {
		t.Fatal("a fresh confirmation must count")
	}
}

func TestButtonsWithoutEmojiSelector(t *testing.T) {
	// some clients send "▶ Resume · id" back without U+FE0F
	if a, id, ok := parseStrategyButton(plainEmoji(strategyButton(i18n.RU, "rb_resume", "funding.carry"))); !ok || a != "resume" || id != "funding.carry" {
		t.Fatalf("resume without FE0F: %q %q %v", a, id, ok)
	}
	if !isButton(plainEmoji(i18n.T(i18n.EN, "btn_menu")), "btn_menu") {
		t.Fatal("menu without FE0F must match")
	}
}

func TestParseStrategyButtonRejects(t *testing.T) {
	for _, text := range []string{"", "hello", i18n.T(i18n.EN, "rb_pause"), i18n.T(i18n.EN, "rb_pause") + idSep, "📊 Status"} {
		if a, id, ok := parseStrategyButton(text); ok {
			t.Errorf("%q parsed as %q %q", text, a, id)
		}
	}
}

func TestReplyKeyboards(t *testing.T) {
	b := &Bot{}
	for _, lang := range []string{i18n.EN, i18n.RU} {
		main := texts(mainKeyboard(lang))
		for _, key := range []string{"btn_status", "btn_bots", "btn_help", "btn_lang"} {
			found := false
			for _, text := range main {
				found = found || (text == i18n.T(lang, key) && isButton(text, key))
			}
			if !found {
				t.Errorf("%s: main keyboard lacks %s: %v", lang, key, main)
			}
		}
		if got := texts(b.botsKeyboard(lang)); len(got) != 2 || !isButton(got[1], "btn_menu") {
			t.Errorf("%s: empty bots keyboard = %v", lang, got)
		}
		if got := texts(langKeyboard(lang)); got[0] != langEN || got[1] != langRU {
			t.Errorf("%s: lang keyboard = %v", lang, got)
		}
	}
}

func TestViews(t *testing.T) {
	b := &Bot{}
	const chat = 42
	if kb := b.keyboardFor(chat, i18n.EN); texts(kb)[0] != i18n.T(i18n.EN, "btn_status") {
		t.Fatalf("default view must be the main menu: %v", texts(kb))
	}
	b.setView(chat, viewStrat+"funding")
	if a, id, ok := parseStrategyButton(texts(b.keyboardFor(chat, i18n.RU))[0]); !ok || a != "pause" || id != "funding" {
		t.Fatalf("strategy view: %q %q %v", a, id, ok)
	}
	b.setView(chat, viewMain)
	if v := b.view(chat); v != viewMain {
		t.Fatalf("view after reset = %q", v)
	}
}
