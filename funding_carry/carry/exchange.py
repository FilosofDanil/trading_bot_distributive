"""Signed Binance client for live carry: spot, USDⓈ-M futures and the universal transfer (carry/live.py Exchange).

Keys come from the environment only: BINANCE_API_KEY plus either BINANCE_API_SECRET (HMAC-SHA256) or
BINANCE_PRIVATE_KEY (an Ed25519 private key in PEM, for a self-generated key). Binance gives an HMAC key without
an IP whitelist read permission only, so on a host without a static IP the key has to be a self-generated one;
`check_permissions` reads GET /sapi/v1/account/apiRestrictions and the runner refuses to trade without spot
trading, futures and universal transfer.

Notes from the API (2025-12): conditional orders (STOP_MARKET) live in the Algo Service
(POST /fapi/v1/algoOrder; /fapi/v1/order answers -4120); the margin type cannot change while a position or an
order exists (-4048 / -4047; -4046 = already that type); a spot BUY pays its taker fee in the bought coin.
"""
from __future__ import annotations

import base64
import hashlib
import hmac
import itertools
import os
import threading
import time
from urllib.parse import quote, urlencode

import httpx

from .live import PERP_FEE, SPOT_FEE, ExchangeError, Fill, Perp, base_asset, floor_step
from .market import FAPI, SPOT, perpetuals, spot_info

RECV_WINDOW = 10_000
ORDER_TAG = "fc"           # newClientOrderId / clientAlgoId prefix: fc-<ms>-<n>, stops fcs-<ms>-<n>
STOP_TAG = "fcs"


class BinanceError(ExchangeError):
    def __init__(self, status: int, code: int | None, msg: str, path: str):
        super().__init__(f"{path}: HTTP {status} code {code}: {msg}")
        self.status, self.code, self.msg = status, code, msg


class _Signer:
    def __init__(self, secret: str | None, private_pem: str | None):
        self.secret = secret or ""
        self.key = None
        if private_pem:
            from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
            from cryptography.hazmat.primitives.serialization import load_pem_private_key
            key = load_pem_private_key(private_pem.replace("\\n", "\n").encode(), password=None)
            if not isinstance(key, Ed25519PrivateKey):
                raise SystemExit("BINANCE_PRIVATE_KEY must be an Ed25519 private key (PEM)")
            self.key = key
        if not self.secret and self.key is None:
            raise SystemExit("live mode needs BINANCE_API_SECRET (HMAC) or BINANCE_PRIVATE_KEY (Ed25519 PEM)")

    def sign(self, payload: str) -> str:
        if self.key is not None:
            return base64.b64encode(self.key.sign(payload.encode())).decode()
        return hmac.new(self.secret.encode(), payload.encode(), hashlib.sha256).hexdigest()


class Binance:
    def __init__(self, api_key: str, secret: str | None = None, private_pem: str | None = None,
                 fapi: str = FAPI, spot: str = SPOT, http: httpx.Client | None = None):
        if not api_key:
            raise SystemExit("live mode needs BINANCE_API_KEY")
        self.api_key = api_key
        self.signer = _Signer(secret, private_pem)
        self.fapi, self.spot = fapi, spot
        self.http = http or httpx.Client(timeout=20)
        self._offset = {fapi: 0, spot: 0}
        self._ids = itertools.count(1)
        self._lock = threading.Lock()
        self._hedge: bool | None = None
        self.sync_time()

    @classmethod
    def from_env(cls) -> "Binance":
        return cls(os.environ.get("BINANCE_API_KEY", "").strip(),
                   os.environ.get("BINANCE_API_SECRET", "").strip() or None,
                   os.environ.get("BINANCE_PRIVATE_KEY", "").strip() or None)

    # ------------------------------------------------------------ transport
    def sync_time(self) -> None:
        for base, path in ((self.fapi, "/fapi/v1/time"), (self.spot, "/api/v3/time")):
            t0 = time.time()
            server = self.http.get(base + path).json()["serverTime"]
            self._offset[base] = int(server - (t0 + time.time()) / 2 * 1000)

    def _cid(self, tag: str) -> str:
        return f"{tag}-{int(time.time() * 1000)}-{next(self._ids)}"[:36]

    def call(self, method: str, base: str, path: str, params: dict | None = None, signed: bool = True):
        for attempt in range(3):
            p = {k: v for k, v in (params or {}).items() if v is not None}
            if signed:
                p["recvWindow"] = RECV_WINDOW
                p["timestamp"] = int(time.time() * 1000) + self._offset[base]
                q = urlencode(p)
                q += "&signature=" + quote(self.signer.sign(q), safe="")   # Ed25519 base64 needs escaping
            else:
                q = urlencode(p)
            try:
                r = self.http.request(method, f"{base}{path}?{q}", headers={"X-MBX-APIKEY": self.api_key})
            except httpx.TransportError as exc:
                if attempt == 2 or method != "GET":
                    raise ExchangeError(f"{method} {path}: {exc}") from exc
                time.sleep(2)
                continue
            if r.status_code < 400:
                return r.json()
            try:
                body = r.json()
            except ValueError:
                body = {}
            code = body.get("code")
            if code == -1021 and attempt < 2:             # clock drift: resync and retry
                self.sync_time()
                continue
            if r.status_code in (429, 418) or r.status_code >= 500:
                if attempt < 2 and method == "GET":
                    time.sleep(5 * (attempt + 1))
                    continue
            raise BinanceError(r.status_code, code, body.get("msg", r.text[:200]), f"{method} {path}")
        raise ExchangeError(f"{method} {path}: retries exhausted")

    # ------------------------------------------------------------ permissions / mode
    def check_permissions(self) -> list[str]:
        """Missing permissions for live carry ([] = ok)."""
        r = self.call("GET", self.spot, "/sapi/v1/account/apiRestrictions")
        need = {"enableReading": "reading", "enableSpotAndMarginTrading": "spot trading",
                "enableFutures": "futures", "permitsUniversalTransfer": "universal transfer"}
        missing = [name for k, name in need.items() if not r.get(k)]
        if r.get("enableWithdrawals"):
            missing.append("withdrawals must be OFF")
        return missing

    def hedge(self) -> bool:
        if self._hedge is None:
            self._hedge = bool(self.call("GET", self.fapi, "/fapi/v1/positionSide/dual")["dualSidePosition"])
        return self._hedge

    # ------------------------------------------------------------ market data
    def prices(self, sym: str) -> tuple[float, float]:
        mark = float(self.call("GET", self.fapi, "/fapi/v1/premiumIndex", {"symbol": sym}, signed=False)["markPrice"])
        last = float(self.call("GET", self.spot, "/api/v3/ticker/price", {"symbol": sym}, signed=False)["price"])
        return mark, last

    def filters(self, sym: str) -> tuple[float, float, float, float]:
        fut = next((s for s in perpetuals() if s["symbol"] == sym), None)
        spt = next((s for s in spot_info()["symbols"] if s["symbol"] == sym), None)
        if fut is None or spt is None:
            raise ExchangeError(f"{sym}: not trading on both spot and futures")
        ss, smin = _steps(spt, spot=True)
        ps, pmin = _steps(fut, spot=False)
        return ss, smin, ps, pmin

    def tick(self, sym: str) -> float:
        fut = next(s for s in perpetuals() if s["symbol"] == sym)
        return next((float(f["tickSize"]) for f in fut["filters"] if f["filterType"] == "PRICE_FILTER"), 0.0)

    # ------------------------------------------------------------ balances / transfer
    def spot_free(self, asset: str) -> float:
        acc = self.call("GET", self.spot, "/api/v3/account", {"omitZeroBalances": "true"})
        return next((float(b["free"]) for b in acc["balances"] if b["asset"] == asset), 0.0)

    def futures_available(self) -> float:
        bal = self.call("GET", self.fapi, "/fapi/v3/balance")
        return next((float(b["availableBalance"]) for b in bal if b["asset"] == "USDT"), 0.0)

    def transfer(self, amount: float, to_futures: bool) -> None:
        self.call("POST", self.spot, "/sapi/v1/asset/transfer",
                  {"type": "MAIN_UMFUTURE" if to_futures else "UMFUTURE_MAIN", "asset": "USDT", "amount": f"{amount:.2f}"})

    # ------------------------------------------------------------ futures
    def prepare_short(self, sym: str) -> None:
        try:
            self.call("POST", self.fapi, "/fapi/v1/marginType", {"symbol": sym, "marginType": "ISOLATED"})
        except BinanceError as exc:
            if exc.code != -4046:                         # -4046: already isolated
                raise
        self.call("POST", self.fapi, "/fapi/v1/leverage", {"symbol": sym, "leverage": 1})

    def perp_order(self, sym: str, side: str, qty: float, reduce: bool) -> Fill:
        p = {"symbol": sym, "side": side, "type": "MARKET", "quantity": _num(qty),
             "newOrderRespType": "RESULT", "newClientOrderId": self._cid(ORDER_TAG)}
        if self.hedge():
            p["positionSide"] = "SHORT"
        elif reduce:
            p["reduceOnly"] = "true"
        r = self.call("POST", self.fapi, "/fapi/v1/order", p)
        q, px = float(r["executedQty"]), float(r["avgPrice"])
        if q <= 0:
            raise ExchangeError(f"{sym}: perp {side} {qty} not filled ({r.get('status')})")
        return Fill(q, px, float(r.get("cumQuote") or q * px) * PERP_FEE)

    def add_margin(self, sym: str, amount: float) -> None:
        self.call("POST", self.fapi, "/fapi/v1/positionMargin",
                  {"symbol": sym, "amount": f"{amount:.2f}", "type": 1, "positionSide": "SHORT" if self.hedge() else "BOTH"})

    def positions(self) -> dict[str, Perp]:
        out: dict[str, Perp] = {}
        for r in self.call("GET", self.fapi, "/fapi/v3/positionRisk"):
            amt = float(r["positionAmt"])
            if amt == 0 or (self.hedge() and r.get("positionSide") != "SHORT"):
                continue
            out[r["symbol"]] = Perp(amt, float(r["entryPrice"]), float(r["markPrice"]),
                                    float(r.get("liquidationPrice") or 0), float(r.get("isolatedMargin") or 0))
        return out

    def stops(self, syms: list[str]) -> dict[str, list[str]]:
        out: dict[str, list[str]] = {}
        for s in syms:                                    # per symbol: weight 1 (40 without a symbol)
            for o in self.call("GET", self.fapi, "/fapi/v1/openAlgoOrders", {"symbol": s}):
                if str(o.get("clientAlgoId", "")).startswith(STOP_TAG + "-"):
                    out.setdefault(s, []).append(str(o["algoId"]))
        return out

    def place_stop(self, sym: str, trigger: float) -> str:
        tick = self.tick(sym)
        trig = floor_step(trigger, tick) if tick else trigger
        p = {"algoType": "CONDITIONAL", "symbol": sym, "side": "BUY", "type": "STOP_MARKET",
             "triggerPrice": _num(trig), "closePosition": "true", "workingType": "MARK_PRICE",
             "positionSide": "SHORT" if self.hedge() else "BOTH", "clientAlgoId": self._cid(STOP_TAG)}
        return str(self.call("POST", self.fapi, "/fapi/v1/algoOrder", p)["algoId"])

    def cancel_stops(self, sym: str) -> None:
        for algo_id in self.stops([sym]).get(sym, []):   # only this bot's stops, never the owner's own
            self.call("DELETE", self.fapi, "/fapi/v1/algoOrder", {"algoId": algo_id})

    def funding(self, sym: str, since_ms: int) -> float:
        rows = self.call("GET", self.fapi, "/fapi/v1/income",
                         {"symbol": sym, "incomeType": "FUNDING_FEE", "startTime": since_ms, "limit": 1000})
        return sum(float(r["income"]) for r in rows)

    # ------------------------------------------------------------ spot
    def spot_order(self, sym: str, side: str, qty: float) -> Fill:
        r = self.call("POST", self.spot, "/api/v3/order",
                      {"symbol": sym, "side": side, "type": "MARKET", "quantity": _num(qty),
                       "newOrderRespType": "FULL", "newClientOrderId": self._cid(ORDER_TAG)})
        executed, quote = float(r["executedQty"]), float(r["cummulativeQuoteQty"])
        if executed <= 0:
            raise ExchangeError(f"{sym}: spot {side} {qty} not filled ({r.get('status')})")
        px = quote / executed
        base = base_asset(sym)
        fee_base = sum(float(f["commission"]) for f in r.get("fills", []) if f["commissionAsset"] == base)
        fee_usdt = sum(float(f["commission"]) for f in r.get("fills", []) if f["commissionAsset"] == "USDT")
        other = any(f["commissionAsset"] not in (base, "USDT") for f in r.get("fills", []))
        fee = fee_base * px + fee_usdt + (quote * SPOT_FEE * 0.75 if other else 0.0)   # BNB: ~25 % off, estimated
        net = executed - fee_base if side == "BUY" else executed
        return Fill(net, px, fee)


def _num(x: float) -> str:
    s = f"{x:.10f}".rstrip("0").rstrip(".")
    return s or "0"


def _steps(info: dict, spot: bool) -> tuple[float, float]:
    step, mn = 0.0, 5.0
    for f in info.get("filters", []):
        if f["filterType"] in ("LOT_SIZE", "MARKET_LOT_SIZE") and float(f.get("stepSize") or 0) > 0:
            step = max(step, float(f["stepSize"]))
        if spot and f["filterType"] in ("NOTIONAL", "MIN_NOTIONAL"):
            mn = float(f.get("minNotional", mn))
        if not spot and f["filterType"] == "MIN_NOTIONAL":
            mn = float(f.get("notional", mn))
    return step, mn
