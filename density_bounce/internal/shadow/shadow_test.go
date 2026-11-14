package shadow_test

import (
	"math"
	"testing"

	"densitybounce/internal/book"
	"densitybounce/internal/setup"
	"densitybounce/internal/shadow"
	. "densitybounce/internal/testkit"
)

const NOW = 1000.0
const (
	ENTRY = 1.0002
	STOP  = 0.9985
	TP    = 1.0200
	TP1   = 1.0019
)

var RISK = ENTRY - STOP
var FEE = shadow.Fee{Maker: 0.0002, Taker: 0.0005}
var bidKey = book.DKey{Side: book.Bid, Tick: 10000}

func setupP(t *testing.T) {
	PinP(t)
	setup.P.CancelAfter, setup.P.MaxHold = 180.0, 3600.0
}

func longPlan(tp1 float64) *setup.Plan {
	return &setup.Plan{Side: "long", Entry: ENTRY, Stop: STOP, TP: TP, DKey: bidKey, TP1: tp1}
}

func mk(plan *setup.Plan) (*book.Book, *shadow.Shadow) {
	if plan == nil {
		plan = longPlan(TP1)
	}
	return LongBook(NOW), shadow.New("XUSDT", plan, NOW, "open", false)
}

type pr struct {
	dt   float64
	tick int32
	bm   bool
}

func tape(b *book.Book, ps ...pr) {
	for _, p := range ps {
		Print(b, NOW+p.dt, p.tick, 1.0, p.bm)
	}
}

var fill = pr{1, 10001, true} // a sell prints through our bid at 1.0002 (tick 10002)

func near(t *testing.T, name string, got *float64, want, tol float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > tol {
		t.Errorf("%s = %v, want %v +- %v", name, got, want, tol)
	}
}

func TestFillThenStopLosesOneRPlusFeesAndTheSlip(t *testing.T) {
	setupP(t)
	b, sh := mk(nil)
	tape(b, fill, pr{5, 9985, true})
	sh.Step(b, NOW+6, FEE)
	out := sh.Out
	if sh.State != "done" || out.Hit != "stop" || *out.FillS != 1.0 || *out.HoldS != 4.0 {
		t.Fatalf("out %+v", out)
	}
	exit := STOP * (1 - shadow.StopSlip)
	want := (exit-ENTRY)/RISK - (FEE.Maker+FEE.Taker)*ENTRY/RISK
	near(t, "r", out.R, want, 0.002)
	if *out.R > -1.0 || *out.R < -1.7 {
		t.Errorf("r %v", *out.R)
	}
	near(t, "mae", out.MaeR, -1.0, 0.01)
	if *out.MfeR != 0 {
		t.Errorf("mfe %v", *out.MfeR)
	}
}

func TestFillThenTakeWithTheHalfAtOneR(t *testing.T) {
	setupP(t)
	b, sh := mk(nil)
	tape(b, fill, pr{5, 10020, false}, pr{9, 10201, false}) // through tp1 (1.0019), then through the take
	sh.Step(b, NOW+10, FEE)
	out := sh.Out
	if out.Hit != "take" || *out.HoldS != 8.0 {
		t.Fatalf("out %+v", out)
	}
	gross := 0.5*(TP1-ENTRY)/RISK + 0.5*(TP-ENTRY)/RISK
	feeR := (FEE.Maker + 0.5*FEE.Maker + 0.5*FEE.Maker) * ENTRY / RISK
	near(t, "r", out.R, gross-feeR, 0.002)
	if *out.R <= 5 {
		t.Errorf("r %v", *out.R)
	}
	near(t, "mfe", out.MfeR, (1.0201-ENTRY)/RISK, 0.02)
}

func TestTakeWithoutTP1IsAllInOnePart(t *testing.T) {
	setupP(t)
	b, sh := mk(longPlan(0))
	tape(b, fill, pr{9, 10201, false})
	sh.Step(b, NOW+10, FEE)
	want := (TP-ENTRY)/RISK - 2*FEE.Maker*ENTRY/RISK
	if sh.Out.Hit != "take" {
		t.Fatal(sh.Out.Hit)
	}
	near(t, "r", sh.Out.R, want, 0.002)
}

func TestTP1ThenStopIsTP1PlusStop(t *testing.T) {
	setupP(t)
	b, sh := mk(nil)
	tape(b, fill, pr{5, 10020, false}, pr{8, 9985, true})
	sh.Step(b, NOW+10, FEE)
	exit := STOP * (1 - shadow.StopSlip)
	gross := 0.5*(TP1-ENTRY)/RISK + 0.5*(exit-ENTRY)/RISK
	feeR := (FEE.Maker + 0.5*FEE.Maker + 0.5*FEE.Taker) * ENTRY / RISK
	if sh.Out.Hit != "tp1+stop" {
		t.Fatal(sh.Out.Hit)
	}
	near(t, "r", sh.Out.R, gross-feeR, 0.002)
}

func TestTimeExitClosesAtTheMidAfterMaxHold(t *testing.T) {
	setupP(t)
	b, sh := mk(nil)
	tape(b, fill)
	sh.Step(b, NOW+100, FEE)
	if sh.State != "open" {
		t.Fatal(sh.State)
	}
	sh.Step(b, NOW+1+3600+1, FEE) // no new prints for an hour: the mid (1.0030) decides
	want := (1.0030-ENTRY)/RISK - (FEE.Maker+FEE.Taker)*ENTRY/RISK
	if sh.Out.Hit != "time" {
		t.Fatal(sh.Out.Hit)
	}
	near(t, "r", sh.Out.R, want, 0.002)
	b, sh = mk(nil)
	tape(b, fill, pr{5, 10020, false})
	sh.Step(b, NOW+3700, FEE)
	if sh.Out.Hit != "tp1+time" {
		t.Errorf("hit %q", sh.Out.Hit)
	}
}

func TestNofillAfterCancelAfter(t *testing.T) {
	setupP(t)
	b, sh := mk(nil)
	tape(b, pr{1, 10002, true}, pr{50, 10005, true}) // a print AT our price does not fill us, nothing goes through
	sh.Step(b, NOW+60, FEE)
	if sh.State != "wait" {
		t.Fatal(sh.State)
	}
	sh.Step(b, NOW+181, FEE) // the plain entry is kept 180 s, then cancelled
	if sh.State != "done" || sh.Out.Hit != "nofill" || sh.Out.R != nil || sh.Out.FillS != nil {
		t.Errorf("out %+v", sh.Out)
	}
}

func TestAPrintThroughThePriceAfterCancelAfterIsTooLate(t *testing.T) {
	setupP(t)
	b, sh := mk(nil)
	tape(b, pr{200, 10001, true})
	sh.Step(b, NOW+201, FEE)
	if sh.Out.Hit != "nofill" {
		t.Error(sh.Out.Hit)
	}
	setup.P.CancelAfter = 60.0
	b, sh = mk(nil)
	tape(b, pr{70, 10001, true})
	sh.Step(b, NOW+71, FEE)
	if sh.Out.Hit != "nofill" {
		t.Error(sh.Out.Hit)
	}
	b, sh = mk(nil)
	tape(b, pr{50, 10001, true}) // inside the shorter window: a fill
	sh.Step(b, NOW+51, FEE)
	if sh.State != "open" {
		t.Error(sh.State)
	}
}

func TestStepIsIncrementalAndAFinishedShadowStaysFinished(t *testing.T) {
	setupP(t)
	b, sh := mk(nil)
	tape(b, fill)
	sh.Step(b, NOW+2, FEE)
	if sh.State != "open" || sh.FillTS != NOW+1 {
		t.Fatal(sh.State, sh.FillTS)
	}
	tape(b, pr{5, 9985, true})
	sh.Step(b, NOW+6, FEE)
	first := sh.Out
	if first.Hit != "stop" {
		t.Fatal(first.Hit)
	}
	tape(b, pr{7, 10201, false})
	sh.Step(b, NOW+8, FEE)
	if sh.Out.Hit != first.Hit || *sh.Out.R != *first.R {
		t.Error("a finished shadow changed")
	}
	sh.Lost()
	if sh.Out.Hit != first.Hit { // lost() does not overwrite a result
		t.Error("lost overwrote")
	}
}

func TestShortPlanIsTheMirror(t *testing.T) {
	setupP(t)
	b := LongBook(NOW)
	plan := &setup.Plan{Side: "short", Entry: 0.9998, Stop: 1.0015, TP: 0.9800, DKey: book.DKey{Side: book.Ask, Tick: 10000}, TP1: 0.9981}
	sh := shadow.New("XUSDT", plan, NOW, "open", false)
	tape(b, pr{1, 9999, false}, pr{5, 9980, true}, pr{9, 9799, true}) // a buy through our ask, tp1, the take
	sh.Step(b, NOW+10, FEE)
	risk := 1.0015 - 0.9998
	gross := 0.5*(0.9998-0.9981)/risk + 0.5*(0.9998-0.98)/risk
	feeR := 2 * FEE.Maker * 0.9998 / risk
	if sh.Out.Hit != "take" {
		t.Fatal(sh.Out.Hit)
	}
	near(t, "r", sh.Out.R, gross-feeR, 0.002)
	sh2 := shadow.New("XUSDT", plan, NOW, "open", false)
	b2 := LongBook(NOW)
	tape(b2, pr{1, 9999, false}, pr{5, 10015, false})
	sh2.Step(b2, NOW+6, FEE)
	if sh2.Out.Hit != "stop" || *sh2.Out.R >= -1.0 {
		t.Errorf("out %+v", sh2.Out)
	}
}

func TestLostAndRecord(t *testing.T) {
	setupP(t)
	_, sh := mk(nil)
	sh.Act, sh.Final = "skip:weak", true
	sh.Plan.Skip, sh.Plan.PID = "weak", "XUSDT-1"
	sh.Plan.Features = setup.Features{HoldS: setup.F64(12.0)}
	sh.Lost()
	if sh.State != "done" || sh.Out.Hit != "lost" || sh.Out.R != nil {
		t.Fatalf("out %+v", sh.Out)
	}
	rec := sh.Record("0.4.5")
	if rec.PID != "XUSDT-1" || rec.Ver != "0.4.5" || rec.Act != "skip:weak" || rec.Skip != "weak" || rec.Side != "long" ||
		rec.Entry != ENTRY || rec.TP1 != TP1 || rec.TS != NOW || rec.Shadow.Hit != "lost" || rec.Features.HoldS == nil || *rec.Features.HoldS != 12.0 {
		t.Errorf("record %+v", rec)
	}
}
