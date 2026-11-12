"""Shadow plans: what the PLAIN entry would have done for every plan the bot saw.

The plain entry is the rule until v0.4.5: a limit at the plan's entry price placed at once, kept for cancel_after
seconds whatever the density does, then the plan's stop, half at tp1, the rest at the take, max_hold. It is
followed on the tape for every plan: the ones the bot traded, the ones it cancelled (and why) and the ones a soft
gate skipped (young / weak / counter_slow / market / thin_stop). The plan journal (Store.plan, GET .../plans) then
answers "what did this gate or this cancel cost or save" on every candidate, not only on the filled trades.

Conservative: an order fills only when a trade prints THROUGH its price; a stop fills at its price - stop_slip.
Fees are the journal's (maker entry and takes, taker stop and time exit). `r` is net, in units of the plan's risk.
"""
from __future__ import annotations

from dataclasses import dataclass, field

from . import setup
from .book import SymbolBook

STOP_SLIP = 0.0002


@dataclass
class Shadow:
    sym: str
    plan: setup.Plan
    t0: float
    act: str                      # what the bot did: skip:<gates> | cancel:<why> | filled | open (not decided yet)
    final: bool                   # `act` will not change any more
    entry: float = 0.0            # the plan's prices when it was made (the trader may move the real entry)
    state: str = "wait"           # wait | open | done
    last_ts: float = 0.0
    fill_ts: float = 0.0
    tp1_done: bool = False
    best: float = 0.0             # furthest price in favour / against since the fill
    worst: float = 0.0
    out: dict = field(default_factory=dict)

    def __post_init__(self):
        self.entry = self.plan.entry
        self.last_ts = self.t0

    # ------------------------------------------------------------------ result
    def _finish(self, hit: str, px: float, ts: float, fee: dict) -> None:
        p, sgn = self.plan, self.plan.sgn
        risk = abs(self.entry - p.stop)
        self.state = "done"
        if hit == "nofill" or risk <= 0:
            self.out = {"hit": hit, "r": None}
            return
        last = sgn * (px - self.entry) / risk                         # the part closed now, in R
        half = 0.5 * sgn * (p.tp1 - self.entry) / risk if self.tp1_done else 0.0
        gross = half + (0.5 if self.tp1_done else 1.0) * last
        taker = hit in ("stop", "time")
        exit_fee = (0.5 * fee["maker"] if self.tp1_done else 0.0) + \
                   (0.5 if self.tp1_done else 1.0) * (fee["taker"] if taker else fee["maker"])
        fee_r = (fee["maker"] + exit_fee) * self.entry / risk
        self.out = {"hit": ("tp1+" if self.tp1_done and hit != "take" else "") + hit,
                    "r": round(gross - fee_r, 3), "fill_s": round(self.fill_ts - self.t0, 1),
                    "hold_s": round(ts - self.fill_ts, 1),
                    "mfe_r": round(sgn * (self.best - self.entry) / risk, 2),
                    "mae_r": round(sgn * (self.worst - self.entry) / risk, 2)}

    # ------------------------------------------------------------------ step
    def step(self, b: SymbolBook, now: float, fee: dict) -> None:
        """Advance over the trades printed since the last step."""
        if self.state == "done":
            return
        p, sgn, P = self.plan, self.plan.sgn, setup.P
        new = []
        for tr in reversed(b.trades):
            if tr.ts <= self.last_ts:
                break
            new.append(tr)
        e_t, s_t, tp_t = b.t(self.entry), b.t(p.stop), b.t(p.tp)
        tp1_t = b.t(p.tp1) if p.tp1 else None
        for tr in reversed(new):
            self.last_ts = tr.ts
            px = b.p(tr.tick)
            if self.state == "wait":
                if tr.ts - self.t0 > P["cancel_after"]:
                    return self._finish("nofill", 0.0, tr.ts, fee)
                if (tr.tick - e_t) * sgn < 0:                         # traded through the entry
                    self.state, self.fill_ts = "open", tr.ts
                    self.best = self.worst = self.entry
                else:
                    continue
            if tr.ts - self.fill_ts > P["max_hold"]:
                return self._finish("time", px, tr.ts, fee)
            if (px - self.best) * sgn > 0:
                self.best = px
            if (px - self.worst) * sgn < 0:
                self.worst = px
            if (tr.tick - s_t) * sgn <= 0:
                return self._finish("stop", p.stop * (1 - sgn * STOP_SLIP), tr.ts, fee)
            if tp1_t is not None and not self.tp1_done and (tr.tick - tp1_t) * sgn > 0:
                self.tp1_done = True
            if (tr.tick - tp_t) * sgn > 0:
                return self._finish("take", p.tp, tr.ts, fee)
        if self.state == "wait" and now - self.t0 > P["cancel_after"]:
            self._finish("nofill", 0.0, now, fee)
        elif self.state == "open" and now - self.fill_ts > P["max_hold"]:
            self._finish("time", b.mid() or self.entry, now, fee)

    def lost(self) -> None:
        """The book is gone (the symbol left the watch list): close the record with what is known."""
        if self.state != "done":
            self.state = "done"
            self.out = {"hit": "lost", "r": None}

    def record(self, ver: str) -> dict:
        p = self.plan
        return {"pid": p.pid, "ver": ver, "ts": round(self.t0, 3), "sym": self.sym, "side": p.side,
                "entry": self.entry, "stop": p.stop, "tp": p.tp, "tp1": p.tp1, "skip": p.skip, "act": self.act,
                "shadow": self.out, "features": p.features}
