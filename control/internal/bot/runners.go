package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"control/internal/guard"
	"control/internal/i18n"
	"control/internal/runners"
)

// Telegram caps a message at 4096 characters; stay below it (escaped text is longer than what is shown).
const maxMsgRunes = 3900

// SetRunners wires the trading-bot registry (nil or empty = the runner commands answer "none configured").
func (b *Bot) SetRunners(r *runners.Registry) { b.rr = r }

// NotifyAll sends text to every allowed Telegram user (a private chat id equals the user id).
func (b *Bot) NotifyAll(text string) {
	for id := range b.allowedIDs {
		b.Notify(id, text)
	}
}

// handleRunnerCommand serves /bots /bot /pause /resume /run /plan /positions /report /events /debug /ping.
func (b *Bot) handleRunnerCommand(ctx context.Context, m *tgbotapi.Message, lang string) {
	chatID := m.Chat.ID
	if b.rr.Empty() {
		b.send(chatID, lang, i18n.T(lang, "runners_none"))
		return
	}
	arg := strings.TrimSpace(m.CommandArguments())
	var by int64
	if m.From != nil {
		by = m.From.ID
	}
	switch m.Command() {
	case "bots":
		b.cmdBots(ctx, chatID, lang)
	case "bot":
		b.cmdBot(ctx, chatID, lang, arg)
	case "pause":
		b.cmdSetDesired(ctx, chatID, lang, by, arg, true)
	case "resume":
		b.cmdSetDesired(ctx, chatID, lang, by, arg, false)
	case "run":
		b.cmdRun(ctx, chatID, lang, arg)
	case "plan":
		b.cmdPlan(ctx, chatID, lang, arg)
	case "positions":
		b.cmdPositions(ctx, chatID, lang, arg)
	case "report":
		b.cmdReport(ctx, chatID, lang, arg)
	case "events":
		query, n := arg, 15
		if f := strings.Fields(arg); len(f) > 0 {
			if v, err := strconv.Atoi(f[len(f)-1]); err == nil {
				query, n = strings.Join(f[:len(f)-1], " "), v
			}
		}
		b.cmdEvents(ctx, chatID, lang, query, min(max(n, 1), 50))
	case "debug":
		b.cmdDebug(chatID, lang, arg)
	case "ping":
		b.cmdPing(ctx, chatID, lang)
	}
}

// handleRunnerCallback serves the rb:<action>:<strategy id> inline buttons.
func (b *Bot) handleRunnerCallback(ctx context.Context, q *tgbotapi.CallbackQuery) {
	chatID := q.Message.Chat.ID
	lang := b.langFor(chatID, q.From)
	if b.rr.Empty() {
		b.send(chatID, lang, i18n.T(lang, "runners_none"))
		return
	}
	parts := strings.SplitN(q.Data, ":", 3)
	if len(parts) != 3 {
		return
	}
	action, id := parts[1], parts[2]
	switch action {
	case "pause":
		b.cmdSetDesired(ctx, chatID, lang, q.From.ID, id, true)
	case "resume":
		b.cmdSetDesired(ctx, chatID, lang, q.From.ID, id, false)
	case "run":
		b.cmdRun(ctx, chatID, lang, id)
	case "plan":
		b.cmdPlan(ctx, chatID, lang, id)
	case "report":
		b.cmdReport(ctx, chatID, lang, id)
	case "pos":
		b.cmdPositions(ctx, chatID, lang, id)
	}
}

// ---- keyboards ----

const maxCallbackData = 64

func btn(text, action, id string) (tgbotapi.InlineKeyboardButton, bool) {
	data := "rb:" + action + ":" + id
	if len(data) > maxCallbackData {
		return tgbotapi.InlineKeyboardButton{}, false
	}
	return tgbotapi.NewInlineKeyboardButtonData(text, data), true
}

type rbtn struct{ text, action string }

// row builds one keyboard row for strategy id, skipping buttons whose callback data would be too long.
func row(id string, buttons ...rbtn) []tgbotapi.InlineKeyboardButton {
	var out []tgbotapi.InlineKeyboardButton
	for _, bt := range buttons {
		if kb, ok := btn(bt.text, bt.action, id); ok {
			out = append(out, kb)
		}
	}
	return out
}

func toggleIcon(s runners.Strategy) string {
	if s.Desired == "paused" {
		return "▶️"
	}
	return "⏸"
}

func toggleAction(s runners.Strategy) string {
	if s.Desired == "paused" {
		return "resume"
	}
	return "pause"
}

func toggleLabel(lang string, s runners.Strategy) string {
	if s.Desired == "paused" {
		return i18n.T(lang, "rb_resume")
	}
	return i18n.T(lang, "rb_pause")
}

func (b *Bot) sendInline(chatID int64, text string, rows [][]tgbotapi.InlineKeyboardButton) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	var kept [][]tgbotapi.InlineKeyboardButton
	for _, r := range rows {
		if len(r) > 0 {
			kept = append(kept, r)
		}
	}
	if len(kept) > 0 {
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(kept...)
	}
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("chat %d: send: %v", chatID, err)
	}
}

// ---- commands ----

type stratRow struct {
	snap runners.RunnerSnap
	s    runners.Strategy
}

func (b *Bot) allStrategies() []stratRow {
	var out []stratRow
	for _, snap := range b.rr.Snapshot() {
		for _, s := range snap.Strategies {
			out = append(out, stratRow{snap, s})
		}
	}
	return out
}

func (b *Bot) cmdBots(ctx context.Context, chatID int64, lang string) {
	lines := []string{i18n.T(lang, "bots_title"), ""}
	var rows [][]tgbotapi.InlineKeyboardButton
	n := 0
	now := time.Now()
	for _, snap := range b.rr.Snapshot() {
		if len(snap.Strategies) == 0 {
			icon, label := runnerIcon(lang, snap)
			lines = append(lines, i18n.T(lang, "bots_runner_only", icon, guard.Escape(snap.Name), label, guard.Escape(snap.LastErr)))
			continue
		}
		for _, s := range snap.Strategies {
			n++
			icon, label := stateIcon(lang, snap, s)
			lines = append(lines, i18n.T(lang, "bots_line", n, icon, guard.Escape(s.ID), modeLabel(lang, s.Mode), label,
				s.OpenPositions, guard.Money(s.Equity), guard.Signed(s.PnlTotal), ago(lang, now, snap.LastOK)))
			num := strconv.Itoa(n)
			rows = append(rows, row(s.ID,
				rbtn{num + " " + toggleIcon(s), toggleAction(s)},
				rbtn{num + " 📋", "plan"},
				rbtn{num + " 📊", "report"}))
		}
	}
	b.sendInline(chatID, joinLines(lines), rows)
}

func (b *Bot) cmdBot(ctx context.Context, chatID int64, lang, query string) {
	c, id, err := b.rr.Find(query)
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	var cur *stratRow
	for _, r := range b.allStrategies() {
		if r.s.ID == id {
			r := r
			cur = &r
		}
	}
	if cur == nil {
		b.sendErr(chatID, lang, id, errors.New("strategy disappeared"))
		return
	}
	s, snap := cur.s, cur.snap
	icon, _ := stateIcon(lang, snap, s)
	_, runnerLabel := runnerIcon(lang, snap)
	text := i18n.T(lang, "bot_card", icon, guard.Escape(s.ID), guard.Escape(s.Name),
		modeLabel(lang, s.Mode), stateLabel(lang, s.State), stateLabel(lang, s.Desired), s.OpenPositions,
		list(s.Book), guard.Money(s.Capital0), guard.Money(s.Equity), guard.Signed(s.PnlTotal), guard.Signed(s.PnlToday),
		dash(s.LastRunDay), dash(s.LastRunAt), dash(s.NextRunAt), dash(s.LastError),
		guard.Escape(c.Name), runnerLabel, ago(lang, time.Now(), snap.LastOK))
	if h := snap.Health; h != nil {
		text += "\n" + i18n.T(lang, "bot_health", guard.Escape(h.Version),
			guard.Human(time.Duration(h.UptimeS)*time.Second, lang), okMark(h.Binance.OK),
			strconv.FormatFloat(h.Binance.LatencyMS, 'f', 0, 64), yn(lang, h.Busy))
	}
	b.sendInline(chatID, text, [][]tgbotapi.InlineKeyboardButton{
		row(id, rbtn{toggleLabel(lang, s), toggleAction(s)}, rbtn{i18n.T(lang, "rb_run"), "run"}),
		row(id, rbtn{i18n.T(lang, "rb_plan"), "plan"}, rbtn{i18n.T(lang, "rb_pos"), "pos"},
			rbtn{i18n.T(lang, "rb_report"), "report"}),
	})
}

func (b *Bot) cmdSetDesired(ctx context.Context, chatID int64, lang string, by int64, query string, pause bool) {
	c, id, err := b.rr.Find(query)
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	var s runners.Strategy
	key := "rr_resumed"
	if pause {
		key = "rr_paused"
		s, err = c.Pause(ctx, id, by)
	} else {
		s, err = c.Resume(ctx, id, by)
	}
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	log.Printf("runner %s: %s by user %d", id, key, by)
	b.send(chatID, lang, i18n.T(lang, key, guard.Escape(id), stateLabel(lang, s.State)))
	b.rr.PollNow(ctx)
}

func (b *Bot) cmdRun(ctx context.Context, chatID int64, lang, query string) {
	c, id, err := b.rr.Find(query)
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	res, err := c.Run(ctx, id)
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	key := "rr_run_started"
	if res.Preview {
		key = "rr_run_preview"
	}
	b.send(chatID, lang, i18n.T(lang, key, guard.Escape(id)))
}

func (b *Bot) cmdPlan(ctx context.Context, chatID int64, lang, query string) {
	c, id, err := b.rr.Find(query)
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	p, err := c.Plan(ctx, id)
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	head := i18n.T(lang, "plan_head", guard.Escape(id), guard.Escape(p.Day), yn(lang, p.Accepted), yn(lang, p.Preview),
		yn(lang, p.Paused), guard.Money(p.Equity), guard.Money(p.Idle), list(p.Book))
	head += "\n" + i18n.T(lang, "plan_lists", list(p.Open), list(p.Close), resizeList(p.Resize), list(p.PairStopped),
		list(p.Rehedge), topList(p.Top), f4(p.MinF8*100))

	var rows []string
	if len(p.Orders) > 0 {
		head += "\n\n" + i18n.T(lang, "plan_orders_title")
		var cells [][]string
		for _, o := range p.Orders {
			cells = append(cells, []string{o.Symbol, leg(o.Spot), leg(o.Perp), guard.Money(o.Notional), okMark(o.OK)})
		}
		rows = table(strings.Split(i18n.T(lang, "hdr_orders"), "|"), cells)
	}
	var tail string
	if len(p.Skipped) > 0 {
		syms := make([]string, 0, len(p.Skipped))
		for s := range p.Skipped {
			syms = append(syms, s)
		}
		sort.Strings(syms)
		lines := []string{"", i18n.T(lang, "plan_skipped", len(syms))}
		for i, s := range syms {
			if i == 10 {
				lines = append(lines, "…")
				break
			}
			lines = append(lines, "• "+guard.Escape(s)+": "+guard.Escape(clipRunes(p.Skipped[s], 80)))
		}
		tail = strings.Join(lines, "\n")
	}
	b.send(chatID, lang, fitPre(head, rows, tail))
}

func (b *Bot) cmdPositions(ctx context.Context, chatID int64, lang, query string) {
	c, id, err := b.rr.Find(query)
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	ps, err := c.Positions(ctx, id)
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	if len(ps) == 0 {
		b.send(chatID, lang, i18n.T(lang, "pos_none", guard.Escape(id)))
		return
	}
	var cells [][]string
	var total float64
	for _, p := range ps {
		total += p.Pnl
		cells = append(cells, []string{p.Symbol, guard.Money(p.N), strconv.Itoa(p.Days), guard.Signed(p.Funding),
			guard.Signed(p.Basis), guard.Signed(p.Fees), guard.Signed(p.Pnl)})
	}
	head := i18n.T(lang, "pos_head", guard.Escape(id), len(ps))
	tail := i18n.T(lang, "pos_total", guard.Signed(total))
	b.send(chatID, lang, fitPre(head, table(strings.Split(i18n.T(lang, "hdr_positions"), "|"), cells), "\n"+tail))
}

func (b *Bot) cmdReport(ctx context.Context, chatID int64, lang, query string) {
	c, id, err := b.rr.Find(query)
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	r, err := c.Report(ctx, id)
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	curve := make([]float64, 0, len(r.Curve))
	for _, p := range r.Curve {
		curve = append(curve, p.Equity)
	}
	b.send(chatID, lang, i18n.T(lang, "report", guard.Escape(id), guard.Escape(r.Start), r.Days, r.ActiveDays,
		guard.Money(r.Capital0), guard.Money(r.Equity), fmt.Sprintf("%+.2f", r.ReturnPct), fmt.Sprintf("%.2f", r.AnnualPct),
		fmt.Sprintf("%.2f", r.MaxDDPct), guard.Signed(r.Funding), guard.Signed(r.Basis), guard.Signed(r.Fees),
		r.PairsOpened, r.PairsClosed, r.PairStops, spark(curve, 30)))
}

func (b *Bot) cmdEvents(ctx context.Context, chatID int64, lang, query string, n int) {
	c, id, err := b.rr.Find(query)
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	evs, err := c.Events(ctx, id, -1, n)
	if err != nil {
		b.sendErr(chatID, lang, id, err)
		return
	}
	if len(evs) == 0 {
		b.send(chatID, lang, i18n.T(lang, "ev_none", guard.Escape(id)))
		return
	}
	if len(evs) > n {
		evs = evs[len(evs)-n:]
	}
	lines := []string{i18n.T(lang, "ev_head", guard.Escape(id), len(evs)), ""}
	for _, e := range evs {
		hhmm := "--:--"
		if t := e.Time(); !t.IsZero() {
			hhmm = t.UTC().Format("15:04")
		}
		lines = append(lines, fmt.Sprintf("<code>%s</code> %s %s", hhmm, runners.LevelIcon(e.Level), guard.Escape(e.Msg)))
	}
	b.send(chatID, lang, joinLines(lines))
}

func (b *Bot) cmdDebug(chatID int64, lang, filter string) {
	var lines []string
	now := time.Now()
	for _, s := range b.rr.Snapshot() {
		if filter != "" && s.Name != filter {
			continue
		}
		lines = append(lines, i18n.T(lang, "dbg_runner", guard.Escape(s.Name), guard.Escape(s.URL), okMark(s.OK),
			s.Latency.Milliseconds(), s.FailStreak, ago(lang, now, s.LastOK), dash(strPtr(s.LastErr))))
		if h := s.Health; h != nil {
			bin := okMark(h.Binance.OK)
			if h.Binance.Error != nil {
				bin += " <code>" + guard.Escape(*h.Binance.Error) + "</code>"
			}
			lines = append(lines, i18n.T(lang, "dbg_health", guard.Escape(h.Version),
				guard.Human(time.Duration(h.UptimeS)*time.Second, lang), modeLabel(lang, h.Mode), bin,
				strconv.FormatFloat(h.Binance.LatencyMS, 'f', 0, 64), strconv.FormatFloat(h.RSSMB, 'f', 1, 64),
				h.UniverseDays, h.StateBytes, yn(lang, h.Busy)))
			if len(h.Errors) > 0 {
				lines = append(lines, i18n.T(lang, "dbg_errors"))
				for _, e := range h.Errors {
					lines = append(lines, "• <code>"+guard.Escape(e.TS)+"</code> "+guard.Escape(clipRunes(e.Msg, 200)))
				}
			}
		} else {
			lines = append(lines, i18n.T(lang, "dbg_nohealth"))
		}
		lines = append(lines, "")
	}
	if len(lines) == 0 {
		var names []string
		for _, s := range b.rr.Snapshot() {
			names = append(names, s.Name)
		}
		b.send(chatID, lang, i18n.T(lang, "rr_error", guard.Escape("unknown runner; known: "+strings.Join(names, ", "))))
		return
	}
	b.send(chatID, lang, joinLines(lines))
}

func (b *Bot) cmdPing(ctx context.Context, chatID int64, lang string) {
	b.rr.PollNow(ctx)
	var lines []string
	for _, s := range b.rr.Snapshot() {
		icon, _ := runnerIcon(lang, s)
		if s.OK {
			lines = append(lines, i18n.T(lang, "ping_line", icon, guard.Escape(s.Name), s.Latency.Milliseconds()))
		} else {
			lines = append(lines, i18n.T(lang, "ping_fail", icon, guard.Escape(s.Name), s.FailStreak, guard.Escape(s.LastErr)))
		}
	}
	b.send(chatID, lang, joinLines(lines))
}

// sendErr turns a runner or lookup error into a user message.
func (b *Bot) sendErr(chatID int64, lang, id string, err error) {
	var he *runners.HTTPError
	switch {
	case errors.As(err, &he) && he.Status == 409:
		b.send(chatID, lang, i18n.T(lang, "rr_busy", guard.Escape(id), guard.Escape(he.Msg)))
	case errors.As(err, &he) && he.Status == 404 && id != "":
		b.send(chatID, lang, i18n.T(lang, "rr_not_found", guard.Escape(id), guard.Escape(he.Msg)))
	default:
		b.send(chatID, lang, i18n.T(lang, "rr_error", guard.Escape(err.Error())))
	}
}

// ---- formatting helpers ----

func runnerStatusKey(s runners.RunnerSnap) (icon, key string) {
	switch {
	case s.FailStreak >= runners.DownAfter:
		return "⚫", "rs_unreachable"
	case s.FailStreak > 0:
		return "🟡", "rs_stale"
	}
	return "🟢", "rs_ok"
}

func runnerIcon(lang string, s runners.RunnerSnap) (icon, label string) {
	icon, key := runnerStatusKey(s)
	return icon, i18n.T(lang, key)
}

// stateIcon is the strategy emoji; a stale or unreachable runner overrides the strategy state.
func stateIcon(lang string, snap runners.RunnerSnap, s runners.Strategy) (icon, label string) {
	if snap.FailStreak > 0 {
		return runnerIcon(lang, snap)
	}
	icons := map[string]string{"running": "🟢", "paused": "⏸", "busy": "⏳", "error": "🔴"}
	icon = icons[s.State]
	if icon == "" {
		icon = "❔"
	}
	return icon, stateLabel(lang, s.State)
}

func stateLabel(lang, state string) string {
	if i18n.Has(lang, "rs_"+state) {
		return i18n.T(lang, "rs_"+state)
	}
	return guard.Escape(state)
}

func modeLabel(lang, mode string) string {
	switch mode {
	case "paper":
		return i18n.T(lang, "mode_paper")
	case "live":
		return i18n.T(lang, "mode_live")
	}
	return guard.Escape(strings.ToUpper(mode))
}

func yn(lang string, v bool) string {
	if v {
		return i18n.T(lang, "yes")
	}
	return i18n.T(lang, "no")
}

func okMark(v bool) string {
	if v {
		return "✅"
	}
	return "❌"
}

func dash(s *string) string {
	if s == nil || *s == "" {
		return "—"
	}
	return guard.Escape(*s)
}

func strPtr(s string) *string { return &s }

func list(ss []string) string {
	if len(ss) == 0 {
		return "—"
	}
	esc := make([]string, len(ss))
	for i, s := range ss {
		esc[i] = guard.Escape(s)
	}
	return strings.Join(esc, ", ")
}

func resizeList(m map[string][2]float64) string {
	if len(m) == 0 {
		return "—"
	}
	syms := make([]string, 0, len(m))
	for s := range m {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	out := make([]string, len(syms))
	for i, s := range syms {
		out[i] = fmt.Sprintf("%s %.1f→%.1f", guard.Escape(s), m[s][0], m[s][1])
	}
	return strings.Join(out, ", ")
}

// topList prints funding rates as % per 8 h with 4 decimals.
func topList(top []runners.FundingRate) string {
	if len(top) == 0 {
		return "—"
	}
	out := make([]string, len(top))
	for i, t := range top {
		out[i] = guard.Escape(t.Symbol) + " " + f4(t.F8*100)
	}
	return strings.Join(out, ", ")
}

func f4(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

func leg(l runners.Leg) string {
	if l.Side == "" {
		return "—"
	}
	return fmt.Sprintf("%s %g", l.Side, l.Qty)
}

// ago renders the time since t ("12s", "5m", "2h 05m"); "—" when t is unset.
func ago(lang string, now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	if d < time.Minute {
		unit := "s"
		if lang == i18n.RU {
			unit = "с"
		}
		return fmt.Sprintf("%d%s", int(d.Seconds()), unit)
	}
	return guard.Human(d, lang)
}

// spark draws the last n values as ▁▂▃▄▅▆▇█ scaled between their min and max.
func spark(vals []float64, n int) string {
	if len(vals) == 0 {
		return "—"
	}
	if len(vals) > n {
		vals = vals[len(vals)-n:]
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo, hi = min(lo, v), max(hi, v)
	}
	bars := []rune("▁▂▃▄▅▆▇█")
	out := make([]rune, len(vals))
	for i, v := range vals {
		idx := len(bars) / 2
		if hi > lo {
			idx = int((v - lo) / (hi - lo) * float64(len(bars)-1))
		}
		out[i] = bars[idx]
	}
	return string(out)
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// table aligns columns (left-aligned, two spaces apart) and returns the lines, header first.
func table(hdr []string, rows [][]string) []string {
	all := append([][]string{hdr}, rows...)
	widths := make([]int, len(hdr))
	for _, r := range all {
		for i, c := range r {
			if i < len(widths) {
				widths[i] = max(widths[i], utf8.RuneCountInString(c))
			}
		}
	}
	out := make([]string, len(all))
	for k, r := range all {
		var sb strings.Builder
		for i, c := range r {
			if i > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(c)
			if i < len(r)-1 {
				sb.WriteString(strings.Repeat(" ", max(0, widths[min(i, len(widths)-1)]-utf8.RuneCountInString(c))))
			}
		}
		out[k] = sb.String()
	}
	return out
}

// fitPre builds head + <pre>rows</pre> + tail within the Telegram limit by dropping whole rows,
// so a cut never lands inside an HTML tag. head and tail must already be valid HTML.
func fitPre(head string, rows []string, tail string) string {
	if len(rows) == 0 {
		return head + tail
	}
	budget := maxMsgRunes - utf8.RuneCountInString(head) - utf8.RuneCountInString(tail) - len("<pre></pre>\n") - 16
	var kept []string
	used := 0
	for i, r := range rows {
		esc := guard.Escape(r)
		used += utf8.RuneCountInString(esc) + 1
		if used > budget {
			kept = append(kept, fmt.Sprintf("… +%d", len(rows)-i))
			break
		}
		kept = append(kept, esc)
	}
	return head + "\n<pre>" + strings.Join(kept, "\n") + "</pre>" + tail
}

// joinLines joins HTML-balanced lines, stopping before the Telegram limit.
func joinLines(lines []string) string {
	var sb strings.Builder
	used := 0
	for i, l := range lines {
		used += utf8.RuneCountInString(l) + 1
		if used > maxMsgRunes {
			sb.WriteString("\n… +" + strconv.Itoa(len(lines)-i))
			break
		}
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(l)
	}
	return sb.String()
}
