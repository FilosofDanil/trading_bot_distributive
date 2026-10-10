package runners

// DownAfter is the number of consecutive failed polls after which a runner counts as unreachable.
const DownAfter = 3

// FailEvent is what a change of the failure streak means for alerting.
type FailEvent int

const (
	FailNone      FailEvent = iota
	FailDown                // just crossed DownAfter: alert once
	FailRecovered           // answers again after being reported down: alert once
)

// FailTransition compares the failure streak before and after a poll.
func FailTransition(prev, cur int) FailEvent {
	switch {
	case prev < DownAfter && cur >= DownAfter:
		return FailDown
	case prev >= DownAfter && cur == 0:
		return FailRecovered
	}
	return FailNone
}

// NewStrategyError reports whether last_error changed to a new non-empty value
// (prev is the last error already seen, "" for none).
func NewStrategyError(prev string, cur *string) (string, bool) {
	if cur == nil || *cur == "" {
		return "", false
	}
	return *cur, *cur != prev
}

// ForwardEvents keeps the events that must reach Telegram (notify=true or level warn/error)
// and returns the highest seq seen among all of them, filtered or not (>= since).
func ForwardEvents(evs []Event, since int64) (out []Event, maxSeq int64) {
	maxSeq = since
	for _, e := range evs {
		if e.Seq <= since {
			continue
		}
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
		if e.Notify || e.Level == "warn" || e.Level == "error" {
			out = append(out, e)
		}
	}
	return out, maxSeq
}
