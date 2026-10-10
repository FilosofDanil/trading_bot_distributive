package bot

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"control/internal/guard"
	"control/internal/i18n"
	"control/internal/store"
)

func TestStatusTextBothLanguages(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.json"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	g := guard.New(guard.Options{Location: time.UTC, Rules: guard.Rules{
		AlertPct: 10, BlockPct: 20, BlockFor: 24 * time.Hour, Period: 24 * time.Hour, AlertResetAfter: time.Hour,
	}}, st, nil)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	if s := StatusText(st, g, 1, i18n.EN, now); !strings.Contains(s, "/start") {
		t.Fatalf("unconfigured: %s", s)
	}
	_, _ = st.Update(1, func(u *store.User) {
		u.Active = true
		u.PeriodStart = now.Add(-2 * time.Hour)
		u.Baseline = 100
		u.Alerted, u.AlertedAt = true, now.Add(-time.Minute)
		u.Last = store.Snapshot{CheckedAt: now.Add(-5 * time.Second), WalletBalance: 100, UnrealizedPnL: -12.5,
			Equity: 87.5, LossAmount: 12.5, LossPct: 12.5, OpenPositions: 2}
	})
	for _, lang := range []string{i18n.EN, i18n.RU} {
		s := StatusText(st, g, 1, lang, now)
		for _, want := range []string{"87.50", "-12.50", "100.00", "-12.50%", "22"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s status missing %q:\n%s", lang, want, s)
			}
		}
		if strings.Contains(s, "%!") {
			t.Errorf("%s status has a format error:\n%s", lang, s)
		}
	}

	_, _ = st.Update(1, func(u *store.User) { u.Blocked, u.BlockedUntil = true, now.Add(3*time.Hour) })
	if s := StatusText(st, g, 1, i18n.RU, now); !strings.Contains(s, "ЗАБЛОКИРОВАНА") || !strings.Contains(s, "3ч 00м") {
		t.Fatalf("blocked RU status:\n%s", s)
	}
}
