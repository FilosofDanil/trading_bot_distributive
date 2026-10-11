package bot

import (
	"context"
	"log"
	"slices"
	"strings"
	"time"

	"control/internal/guard"
	"control/internal/i18n"
	"control/internal/store"
)

// LossGuard and a live carry bot share the futures account (guard/carry.go). These hooks connect them:
// the guard leaves the carry's symbols alone, and a LossGuard block pauses the carry's new entries
// until the block ends (held pairs stay hedged and managed by the carry bot).

const blockReason = "LossGuard block"

// CarrySymbols are the symbols live carry strategies hold now (guard.Options.Exempt).
func (b *Bot) CarrySymbols() map[string]bool {
	if b.rr.Empty() {
		return nil
	}
	return b.rr.LiveSymbols()
}

// OnGuardBlock pauses every running live strategy and remembers which ones it paused.
func (b *Bot) OnGuardBlock(u store.User) {
	if b.rr.Empty() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var paused []string
	for _, r := range b.allStrategies() {
		if r.s.Mode != "live" || r.s.Desired == "paused" {
			continue
		}
		c := b.rr.Client(r.snap.Name)
		if c == nil {
			continue
		}
		if _, err := c.SetDesired(ctx, r.s.ID, true, 0, blockReason); err != nil {
			log.Printf("runner %s: pause on block: %v", r.s.ID, err)
			b.Notify(u.ChatID, i18n.T(u.Lang, "rr_error", guard.Escape(r.s.ID+": "+err.Error())))
			continue
		}
		paused = append(paused, r.snap.Name+"/"+r.s.ID)
	}
	if len(paused) == 0 {
		return
	}
	_, _ = b.st.Update(u.ChatID, func(u *store.User) {
		for _, p := range paused {
			if !slices.Contains(u.PausedCarry, p) {
				u.PausedCarry = append(u.PausedCarry, p)
			}
		}
	})
	log.Printf("carry paused by the LossGuard block: %v", paused)
	b.Notify(u.ChatID, i18n.T(u.Lang, "carry_paused_by_block", guard.Escape(ids(paused))))
	b.rr.PollNow(ctx)
}

// OnGuardUnblock resumes the strategies OnGuardBlock paused (not those the owner paused).
func (b *Bot) OnGuardUnblock(u store.User) {
	cur, _ := b.st.Get(u.ChatID)
	if len(cur.PausedCarry) == 0 || b.rr.Empty() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var resumed, failed []string
	for _, p := range cur.PausedCarry {
		runner, id, _ := strings.Cut(p, "/")
		c := b.rr.Client(runner)
		if c == nil {
			continue // runner removed from the config: nothing to resume
		}
		if _, err := c.SetDesired(ctx, id, false, 0, "LossGuard block ended"); err != nil {
			log.Printf("runner %s: resume after block: %v", id, err)
			failed = append(failed, p)
			continue
		}
		resumed = append(resumed, p)
	}
	_, _ = b.st.Update(u.ChatID, func(u *store.User) { u.PausedCarry = failed })
	if len(resumed) > 0 {
		b.Notify(u.ChatID, i18n.T(u.Lang, "carry_resumed_after_block", guard.Escape(ids(resumed))))
	}
	if len(failed) > 0 {
		b.Notify(u.ChatID, i18n.T(u.Lang, "carry_resume_failed", guard.Escape(ids(failed))))
	}
	b.rr.PollNow(ctx)
}

func ids(refs []string) string {
	out := make([]string, len(refs))
	for i, r := range refs {
		_, id, _ := strings.Cut(r, "/")
		out[i] = id
	}
	return strings.Join(out, ", ")
}
