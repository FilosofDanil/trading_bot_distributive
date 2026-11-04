"""LiveBroker: real orders on Binance USDⓈ-M (from the lab's lab/book/live.py, keys from the environment).

Safety rules:
- trades only symbols with NO position and NO open order / algo order the bot did not place (refreshed every
  30 s), never BTCUSDT / ETHUSDT; every client id starts with "dbb" and cancel / flatten only touch those ids,
  so the carry bot ("fc"), the lab scalper ("skb") and manual trading are never touched;
- isolated margin, LEVERAGE (2), NOTIONAL (10 USDT); the entry is a post-only (GTX) limit;
- the moment an entry fills: an exchange-side STOP_MARKET closePosition algo order (it closes whatever is left)
  + one reduce-only limit take per part (tp1 / tp2 / run); if the stop cannot be placed the position is closed
  at market at once; a shrinking position is checked against the takes, and the trader moves the stop;
- the bot's open slots are kept in the state file; on start leftovers are cancelled / closed.

Keys: BINANCE_API_KEY + BINANCE_API_SECRET (HMAC) or BINANCE_PRIVATE_KEY (Ed25519 PEM). Binance gives an HMAC
key without an IP whitelist read permission only, so on Railway Hobby (no static IP) it has to be an Ed25519 key;
`check()` refuses to trade (mode live-blocked) when futures trading is not enabled or withdrawals are on.
"""
from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import logging
import os
import time
from decimal import Decimal
from typing import Any
from urllib.parse import quote, urlencode

import httpx

from .trader import FEE, RISK, Slot, stop_reason

log = logging.getLogger("bounce")
FAPI = "https://fapi.binance.com"
SAPI = "https://api.binance.com"
PREFIX = "dbb"
NEVER = {"BTCUSDT", "ETHUSDT"}


class BinanceError(Exception):
    def __init__(self, code: int, msg: str, status: int = 0):
        super().__init__(f"{code} {msg}")
        self.code, self.msg, self.status = code, msg, status


def decimals(x: float) -> int:
    return max(0, -int(Decimal(str(x)).normalize().as_tuple().exponent))


def fmt(x: float, step: float) -> str:
    """Round to the grid of `step` and format without float noise."""
    n = round(x / step)
    return f"{n * step:.{decimals(step)}f}"


class Rest:
    """Signed async REST client (HMAC-SHA256 or Ed25519)."""

    def __init__(self, api_key: str, secret: str | None = None, private_pem: str | None = None,
                 http: httpx.AsyncClient | None = None, fapi: str = FAPI, sapi: str = SAPI):
        if not api_key:
            raise SystemExit("live mode needs BINANCE_API_KEY")
        self.key, self.secret, self.ed = api_key, secret or "", None
        if private_pem:
            from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
            from cryptography.hazmat.primitives.serialization import load_pem_private_key
            k = load_pem_private_key(private_pem.replace("\\n", "\n").encode(), password=None)
            if not isinstance(k, Ed25519PrivateKey):
                raise SystemExit("BINANCE_PRIVATE_KEY must be an Ed25519 private key (PEM)")
            self.ed = k
        if not self.secret and self.ed is None:
            raise SystemExit("live mode needs BINANCE_API_SECRET (HMAC) or BINANCE_PRIVATE_KEY (Ed25519 PEM)")
        self.http = http or httpx.AsyncClient(timeout=10)
        self.fapi, self.sapi = fapi, sapi
        self.offset = 0
        self.weight = 0

    @classmethod
    def from_env(cls) -> "Rest":
        return cls(os.environ.get("BINANCE_API_KEY", "").strip(),
                   os.environ.get("BINANCE_API_SECRET", "").strip() or None,
                   os.environ.get("BINANCE_PRIVATE_KEY", "").strip() or None)

    def new_http(self) -> None:
        """A fresh connection pool (an AsyncClient is bound to the event loop that first used it)."""
        self.http = httpx.AsyncClient(timeout=10)

    def sign(self, payload: str) -> str:
        if self.ed is not None:
            return base64.b64encode(self.ed.sign(payload.encode())).decode()
        return hmac.new(self.secret.encode(), payload.encode(), hashlib.sha256).hexdigest()

    async def sync_time(self) -> None:
        r = await self.http.get(self.fapi + "/fapi/v1/time")
        self.offset = int(r.json()["serverTime"]) - int(time.time() * 1000)

    async def call(self, method: str, path: str, signed: bool = True, base: str | None = None, **params) -> Any:
        base = base or self.fapi
        for attempt in range(2):
            p = {k: v for k, v in params.items() if v is not None}
            if signed:
                p.update(timestamp=int(time.time() * 1000) + self.offset, recvWindow=5000)
                q = urlencode(p)
                q += "&signature=" + quote(self.sign(q), safe="")
            else:
                q = urlencode(p)
            r = await self.http.request(method, f"{base}{path}?{q}", headers={"X-MBX-APIKEY": self.key})
            self.weight = int(r.headers.get("X-MBX-USED-WEIGHT-1M", self.weight) or 0)
            if r.status_code == 200:
                return r.json()
            try:
                body = r.json()
                code, msg = int(body.get("code", 0)), str(body.get("msg", ""))
            except Exception:
                code, msg = 0, r.text[:200]
            if code == -1021 and attempt == 0:          # timestamp outside recvWindow: resync and retry once
                await self.sync_time()
                continue
            raise BinanceError(code, msg, r.status_code)
        raise BinanceError(-1021, "timestamp outside recvWindow after a resync")


async def check(rest: Rest) -> list[str]:
    """What prevents live trading ([] = ok): key permissions, position mode, balance."""
    await rest.sync_time()
    problems = []
    try:
        ar = await rest.call("GET", "/sapi/v1/account/apiRestrictions", base=rest.sapi)
        if not ar.get("enableFutures"):
            problems.append("futures trading is not enabled on the key (an HMAC key without an IP whitelist is "
                            "read-only: use a self-generated Ed25519 key, BINANCE_PRIVATE_KEY)")
        if ar.get("enableWithdrawals"):
            problems.append("withdrawals must be OFF on the key")
    except Exception as e:
        problems.append(f"cannot read the key's permissions ({type(e).__name__}: {e})")
        return problems
    if (await rest.call("GET", "/fapi/v1/positionSide/dual")).get("dualSidePosition"):
        problems.append("the account is in hedge mode; the bot needs one-way mode")
    acc = await rest.call("GET", "/fapi/v2/account")
    margin = RISK["max_open"] * RISK["notional"] / RISK["leverage"]    # isolated margin of every trade at once
    if float(acc["availableBalance"]) < margin:
        problems.append(f"futures available balance {float(acc['availableBalance']):.2f} < {margin:g} USDT "
                        f"({RISK['max_open']} x {RISK['notional']:g} USDT at x{RISK['leverage']})")
    return problems


class LiveBroker:
    def __init__(self, rest: Rest):
        self.trader: Any = None                   # set by Trader()
        self.rest = rest
        self.foreign: set[str] = set(NEVER)
        self.configured: set[str] = set()
        self.tasks: list[asyncio.Task] = []
        self.seq = 0
        self.unknown: dict[str, dict] = {}        # entries whose placement result was lost (network)
        self.issued: dict[str, float] = {}        # client id -> time issued

    # ------------------------------------------------------------------ ids / state
    def cid(self, kind: str) -> str:
        self.seq += 1
        c = f"{PREFIX}{kind}{int(time.time() * 1000) % 10**11}{self.seq % 1000:03d}"   # <= 36 chars
        self.issued[c] = time.time()
        return c

    def save_state(self) -> None:
        st = {}
        for sym, s in self.trader.slots.items():
            st[sym] = {"side": s.plan.side, "state": s.state, "qty": s.filled_qty or s.qty, "entry": s.entry_px,
                       "oid": s.oid, "stop_id": s.extra.get("stop_id"), "tp_ids": [p["id"] for p in s.parts],
                       "placed_at": s.placed_at}
        self.trader.store.set("slots", st)

    # ------------------------------------------------------------------ lifecycle
    async def start(self) -> None:
        await self.reconcile()
        await self.refresh_foreign()
        self.tasks.append(asyncio.get_event_loop().create_task(self._foreign_loop()))

    async def refresh_foreign(self) -> None:
        pos = await self.rest.call("GET", "/fapi/v2/positionRisk")
        ours = set(self.trader.slots)
        f = set(NEVER)
        f |= {p["symbol"] for p in pos if float(p["positionAmt"]) != 0 and p["symbol"] not in ours}
        oo = await self.rest.call("GET", "/fapi/v1/openOrders")
        f |= {o["symbol"] for o in oo if not str(o.get("clientOrderId", "")).startswith(PREFIX)}
        ao = await self.rest.call("GET", "/fapi/v1/openAlgoOrders")
        f |= {o["symbol"] for o in ao if not str(o.get("clientAlgoId", "")).startswith(PREFIX)}
        if f != self.foreign:
            log.info("foreign symbols (never traded): %s", " ".join(sorted(f)))
        self.foreign = f
        self.trader.blocked_syms = f

    async def _foreign_loop(self) -> None:
        while True:
            await asyncio.sleep(30)
            try:
                await self.sweep()
                await self.refresh_foreign()
            except Exception as e:
                log.warning("foreign refresh / sweep: %r", e)

    async def sweep(self) -> None:
        """Cancel bot orders that belong to no live slot; close positions left by entries of unknown state."""
        now = time.time()
        live_ids = {c for c, t in self.issued.items() if now - t < 60}   # just issued: maybe not recorded yet
        self.issued = {c: t for c, t in self.issued.items() if now - t < 3600}
        for s in self.trader.slots.values():
            live_ids |= {s.oid, s.extra.get("stop_id")} | {p["id"] for p in s.parts}
        for o in await self.rest.call("GET", "/fapi/v1/openOrders"):
            c = str(o.get("clientOrderId", ""))
            if c.startswith(PREFIX) and c not in live_ids:
                log.warning("sweep: cancelling orphan order %s %s", o["symbol"], c)
                await self._cancel_order(o["symbol"], c)
        for o in await self.rest.call("GET", "/fapi/v1/openAlgoOrders"):
            c = str(o.get("clientAlgoId", ""))
            if c.startswith(PREFIX) and c not in live_ids:
                log.warning("sweep: cancelling orphan algo order %s %s", o["symbol"], c)
                await self._cancel_algo(c)
        for sym, r in list(self.unknown.items()):
            if sym not in self.trader.slots:
                await close_own_position(self.rest, sym, r["side"], r["qty"])
            del self.unknown[sym]

    async def reconcile(self) -> None:
        """Cancel leftover bot orders; close leftover bot positions recorded in the state file."""
        n = await cancel_own_orders(self.rest)
        st = self.trader.store.data.get("slots") or {}
        for sym, r in st.items():
            if r.get("state") in ("open", "closing", "entry"):
                await close_own_position(self.rest, sym, r["side"], float(r["qty"]))
        if n or st:
            self.trader.store.event("warn", f"restart: cancelled {n} leftover bot order(s), closed leftovers on "
                                            f"{', '.join(st) or 'no symbol'}", True)
        self.trader.store.set("slots", {})

    async def ensure_symbol(self, sym: str) -> None:
        if sym in self.configured:
            return
        try:
            await self.rest.call("POST", "/fapi/v1/marginType", symbol=sym, marginType="ISOLATED")
        except BinanceError as e:
            if e.code != -4046:                          # -4046 = already isolated
                raise
        await self.rest.call("POST", "/fapi/v1/leverage", symbol=sym, leverage=RISK["leverage"])
        self.configured.add(sym)

    # ------------------------------------------------------------------ entry
    async def place_entry(self, s: Slot) -> None:
        if s.sym in self.foreign or s.sym in NEVER:
            raise RuntimeError(f"{s.sym} has a foreign position/order")
        i = self.trader.info[s.sym]
        await self.ensure_symbol(s.sym)
        s.oid = self.cid("e")
        side = "BUY" if s.plan.side == "long" else "SELL"
        self.save_state()
        try:
            r = await self.rest.call("POST", "/fapi/v1/order", symbol=s.sym, side=side, type="LIMIT", timeInForce="GTX",
                                     price=fmt(s.plan.entry, i["tick"]), quantity=fmt(s.qty, i["step"]),
                                     newClientOrderId=s.oid, newOrderRespType="RESULT")
        except BinanceError:
            raise                                            # a definite rejection: nothing was placed
        except Exception as e:                               # network: the order may exist -> find it
            log.warning("%s entry request failed (%r); looking the order up", s.sym, e)
            r = None
            for _ in range(5):
                await asyncio.sleep(1.0)
                try:
                    r = await self.rest.call("GET", "/fapi/v1/order", symbol=s.sym, origClientOrderId=s.oid)
                    break
                except BinanceError as be:
                    if be.code == -2013:                     # order does not exist: it was never placed
                        raise RuntimeError("entry not placed") from e
                except Exception:
                    continue
            if r is None:
                self.unknown[s.sym] = {"side": s.plan.side, "qty": s.qty}
                raise RuntimeError("entry state unknown; the leftover sweep will cancel / close it") from e
        if r.get("status") == "EXPIRED" and float(r.get("executedQty", 0)) == 0:
            raise RuntimeError("post-only expired (would cross)")
        self.tasks = [t for t in self.tasks if not t.done()]
        self.tasks.append(asyncio.get_event_loop().create_task(self._poll_entry(s)))

    async def _poll_entry(self, s: Slot) -> None:
        cancel_asked = False
        while s.state == "entry" and self.trader.slots.get(s.sym) is s:
            await asyncio.sleep(0.4)
            try:
                o = await self.rest.call("GET", "/fapi/v1/order", symbol=s.sym, origClientOrderId=s.oid)
            except Exception as e:
                log.warning("poll entry %s: %r", s.sym, e)
                continue
            st, filled = o["status"], float(o["executedQty"])
            if st == "PARTIALLY_FILLED" and not cancel_asked:
                cancel_asked = True                          # take what we got, protect it
                await self._cancel_order(s.sym, s.oid)
                continue
            if st in ("FILLED", "CANCELED", "EXPIRED", "EXPIRED_IN_MATCH", "REJECTED"):
                avg = float(o.get("avgPrice") or 0)
                self.trader.on_entry_result(s.sym, filled, avg, avg * filled * FEE["maker"])
                self.save_state()
                return

    async def _cancel_order(self, sym: str, cid: str) -> None:
        try:
            await self.rest.call("DELETE", "/fapi/v1/order", symbol=sym, origClientOrderId=cid)
        except BinanceError as e:
            if e.code not in (-2011, -2013):                 # unknown order = already filled / cancelled
                raise

    async def cancel_entry(self, s: Slot) -> None:
        await self._cancel_order(s.sym, s.oid)               # _poll_entry reports the final result

    # ------------------------------------------------------------------ protection
    async def _place_stop(self, s: Slot, price: float) -> str:
        i = self.trader.info[s.sym]
        caid = self.cid("s")
        r = await self.rest.call("POST", "/fapi/v1/algoOrder", algoType="CONDITIONAL", symbol=s.sym,
                                 side="SELL" if s.plan.side == "long" else "BUY", type="STOP_MARKET",
                                 triggerPrice=fmt(price, i["tick"]), closePosition="true",
                                 workingType="CONTRACT_PRICE", priceProtect="false", clientAlgoId=caid)
        if r.get("algoStatus") not in ("NEW", None):
            raise RuntimeError(f"stop status {r.get('algoStatus')}")
        return caid

    async def _cancel_algo(self, caid: str | None) -> None:
        if not caid:
            return
        try:
            await self.rest.call("DELETE", "/fapi/v1/algoOrder", clientAlgoId=caid)
        except BinanceError as e:
            log.info("cancel algo %s: %s", caid, e)

    async def protect(self, s: Slot) -> None:
        # watch the position first: if protection fails, the trader's market close is reported by this poll
        self.tasks = [t for t in self.tasks if not t.done()]
        self.tasks.append(asyncio.get_event_loop().create_task(self._poll_open(s)))
        try:
            s.extra["stop_id"] = await self._place_stop(s, s.plan.stop)
        except BinanceError as e:
            if e.code == -2021:                                # would trigger immediately: price is past the stop
                raise
            await asyncio.sleep(0.3)
            s.extra["stop_id"] = await self._place_stop(s, s.plan.stop)   # one retry, then the trader flattens
        self.save_state()
        i = self.trader.info[s.sym]
        for p in s.parts:
            tp_id = self.cid("t")
            try:
                await self.rest.call("POST", "/fapi/v1/order", symbol=s.sym,
                                     side="SELL" if s.plan.side == "long" else "BUY", type="LIMIT", timeInForce="GTC",
                                     price=fmt(p["px"], i["tick"]), quantity=fmt(p["qty"], i["step"]),
                                     reduceOnly="true", newClientOrderId=tp_id)
                p["id"] = tp_id
            except BinanceError as e:
                log.warning("%s take %s not placed (%s); the stop protects the position", s.sym, p["name"], e)
        self.save_state()

    async def _check_parts(self, s: Slot, cancel: bool = False) -> None:
        """Report the takes that filled; with `cancel` (the position is flat) cancel the ones that did not."""
        for p in s.parts:
            if p["done"] or not p["id"]:
                continue
            try:
                o = await self.rest.call("GET", "/fapi/v1/order", symbol=s.sym, origClientOrderId=p["id"])
                if o["status"] == "FILLED":
                    self.trader.on_part(s.sym, p["name"], float(o["executedQty"]), float(o.get("avgPrice") or p["px"]))
                elif cancel:
                    await self._cancel_order(s.sym, p["id"])
            except Exception as e:
                log.warning("take check %s %s: %r", s.sym, p["name"], e)

    async def move_stop(self, s: Slot, price: float) -> None:
        # Binance keeps one closePosition stop per side (-4130), so the old one goes first; if the new one fails, the
        # old one comes back, and if that fails too the position is closed: it never stays without a stop
        old_px = s.stop or s.plan.stop
        await self._cancel_algo(s.extra.get("stop_id"))
        try:
            s.extra["stop_id"] = await self._place_stop(s, price)
        except Exception as e:
            code = getattr(e, "code", None)
            if code == -4130:                                  # the cancel failed: the old stop still stands
                raise
            try:
                s.extra["stop_id"] = await self._place_stop(s, old_px) if code != -2021 else ""
            except Exception as e2:
                log.warning("%s stop not restored (%r): closing", s.sym, e2)
                await self.trader.close(s, "stop_lost")
            self.save_state()
            raise                                              # -2021: price is past the new stop, the trader closes
        self.save_state()

    async def close(self, s: Slot, why: str) -> None:
        i = self.trader.info[s.sym]
        for p in s.parts:
            if p["id"] and not p["done"]:
                await self._cancel_order(s.sym, p["id"])
        amt = await position_amt(self.rest, s.sym)
        if amt != 0:
            await self.rest.call("POST", "/fapi/v1/order", symbol=s.sym, side="SELL" if amt > 0 else "BUY",
                                 type="MARKET", quantity=fmt(min(abs(amt), s.filled_qty), i["step"]),
                                 reduceOnly="true", newClientOrderId=self.cid("x"))
        # _poll_open sees the flat position, cancels the stop and reports the result

    async def _poll_open(self, s: Slot) -> None:
        zeros = 0
        left = s.filled_qty
        step = self.trader.info[s.sym]["step"]
        while self.trader.slots.get(s.sym) is s:
            await asyncio.sleep(1.0)
            try:
                amt = await position_amt(self.rest, s.sym)
            except Exception as e:
                log.warning("poll position %s: %r", s.sym, e)
                continue
            if 0 < abs(amt) < left - step / 2:               # smaller: a take filled (partly)
                left = abs(amt)
                await self._check_parts(s)
            zeros = zeros + 1 if amt == 0 else 0
            if zeros < 2:                                     # two flat readings in a row: no stale-read race
                continue
            await self._cancel_algo(s.extra.get("stop_id"))
            await self._check_parts(s, cancel=True)
            reason = s.extra.get("exit_reason", "")
            if not reason:
                reason = "take" if s.parts and all(p["done"] for p in s.parts) else stop_reason(s)
            exit_px, pnl, fees = await self._realized(s)
            self.trader.on_closed(s.sym, exit_px, pnl, fees, reason)
            self.save_state()
            return

    async def _realized(self, s: Slot) -> tuple[float, float, float]:
        """Exit price, net pnl and fees of this trade from the account's own fills."""
        try:
            tr = await self.rest.call("GET", "/fapi/v1/userTrades", symbol=s.sym,
                                      startTime=int((s.placed_at - 5) * 1000), limit=100)
            exit_side = "SELL" if s.plan.side == "long" else "BUY"
            ex = [t for t in tr if t["side"] == exit_side]
            q = sum(float(t["qty"]) for t in ex)
            px = sum(float(t["qty"]) * float(t["price"]) for t in ex) / q if q else 0.0
            fees = sum(float(t["commission"]) for t in tr if t.get("commissionAsset") == "USDT")
            pnl = sum(float(t["realizedPnl"]) for t in tr) - fees
            return px, pnl, fees
        except Exception as e:
            log.warning("realized %s: %r (pnl estimated)", s.sym, e)
            return s.entry_px, -s.entry_fee, s.entry_fee


# ---------------------------------------------------------------------- helpers shared with reconcile
async def position_amt(rest: Rest, sym: str) -> float:
    pos = await rest.call("GET", "/fapi/v2/positionRisk", symbol=sym)
    return sum(float(p["positionAmt"]) for p in pos)


async def cancel_own_orders(rest: Rest) -> int:
    n = 0
    for o in await rest.call("GET", "/fapi/v1/openOrders"):
        if str(o.get("clientOrderId", "")).startswith(PREFIX):
            try:
                await rest.call("DELETE", "/fapi/v1/order", symbol=o["symbol"], origClientOrderId=o["clientOrderId"])
                n += 1
            except BinanceError as e:
                log.info("cancel %s: %s", o["clientOrderId"], e)
    for o in await rest.call("GET", "/fapi/v1/openAlgoOrders"):
        if str(o.get("clientAlgoId", "")).startswith(PREFIX):
            try:
                await rest.call("DELETE", "/fapi/v1/algoOrder", clientAlgoId=o["clientAlgoId"])
                n += 1
            except BinanceError as e:
                log.info("cancel algo %s: %s", o["clientAlgoId"], e)
    return n


async def close_own_position(rest: Rest, sym: str, side: str, qty: float) -> None:
    """Close at most `qty` of the bot's recorded position on `sym` (reduce-only, never flips, never more)."""
    if sym in NEVER:
        return
    amt = await position_amt(rest, sym)
    if amt == 0 or (amt > 0) != (side == "long"):
        return
    info = await rest.call("GET", "/fapi/v1/exchangeInfo", signed=False)
    step = next(float(f["stepSize"]) for s in info["symbols"] if s["symbol"] == sym
                for f in s["filters"] if f["filterType"] == "LOT_SIZE")
    q = min(abs(amt), qty)
    await rest.call("POST", "/fapi/v1/order", symbol=sym, side="SELL" if amt > 0 else "BUY", type="MARKET",
                    quantity=fmt(q, step), reduceOnly="true", newClientOrderId=f"{PREFIX}x{int(time.time())}")
    log.warning("closed leftover bot position %s %s %g", sym, side, q)
