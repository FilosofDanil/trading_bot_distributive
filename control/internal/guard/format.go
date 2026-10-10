package guard

import (
	"fmt"
	"html"
	"time"

	"control/internal/i18n"
)

func money(v float64) string  { return fmt.Sprintf("%.2f", v) }
func signed(v float64) string { return fmt.Sprintf("%+.2f", v) }
func pct(v float64) string    { return fmt.Sprintf("%.2f", v) }
func escape(s string) string  { return html.EscapeString(s) }

// Money, Signed, Pct, Human and Escape are shared with the bot's /status view.
func Money(v float64) string  { return money(v) }
func Signed(v float64) string { return signed(v) }
func Pct(v float64) string    { return fmt.Sprintf("%+.2f", -v) } // loss 12.5 -> "-12.50"
func Escape(s string) string  { return escape(s) }

// Human renders a duration as "5h 07m" / "5ч 07м".
func Human(d time.Duration, lang string) string { return human(d, lang) }

func human(d time.Duration, lang string) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	hu, mu := "h", "m"
	if lang == i18n.RU {
		hu, mu = "ч", "м"
	}
	if h == 0 {
		return fmt.Sprintf("%d%s", m, mu)
	}
	return fmt.Sprintf("%d%s %02d%s", h, hu, m, mu)
}

// FmtTime renders t in the configured time zone.
func (g *Guard) FmtTime(t time.Time) string { return g.fmtTime(t) }
