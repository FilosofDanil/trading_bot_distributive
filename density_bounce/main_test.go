package main

import (
	"io"
	"path/filepath"
	"testing"

	"densitybounce/internal/state"
	"densitybounce/internal/testkit"
)

func TestAPaperStartForgetsAnOldLiveError(t *testing.T) {
	testkit.PinP(t)
	testkit.PinRisk(t)
	path := filepath.Join(t.TempDir(), "s.json")
	old, err := state.Open(path, 300)
	if err != nil {
		t.Fatal(err)
	}
	old.Out = io.Discard
	old.Event("error", "bounce.density: live trading is OFF - futures available balance 365.17 < 500 USDT", false)
	t.Setenv("MODE", "paper")
	t.Setenv("STATE_FILE", path)
	t.Setenv("PARAMS", "")
	r := build()
	st := r.Strategy()
	if st.LastError != nil || st.State != "running" || st.Mode != "paper" {
		t.Fatalf("%+v", st)
	}
	if len(r.Store.Errors()) == 0 { // the history stays
		t.Fatal("the error history was dropped")
	}
}
