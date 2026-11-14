package keys

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// control plays control's keyserver: code is the status it answers to an authorised request.
func control(t *testing.T) (code *atomic.Int32, hits *atomic.Int32) {
	t.Helper()
	code, hits = new(atomic.Int32), new(atomic.Int32)
	code.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		ok := r.URL.Path == "/v1/binance-key" && r.Header.Get("X-Control-Token") == "tok"
		c := int(code.Load())
		if !ok {
			c = 401
		}
		if c == 200 {
			w.Write([]byte(`{"api_key":"K","api_secret":"S","chat_id":1}`))
			return
		}
		w.WriteHeader(c)
		w.Write([]byte(`{"error":"nope"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BINANCE_API_KEY", "")
	t.Setenv("BINANCE_API_SECRET", "")
	t.Setenv("KEY_FROM_CONTROL", srv.URL+"/")
	t.Setenv("CONTROL_TOKEN", "tok")
	return code, hits
}

func TestKeyLandsInTheEnvironment(t *testing.T) {
	control(t)
	if why := FromControl(1, 0); why != "" {
		t.Fatal(why)
	}
	if os.Getenv("BINANCE_API_KEY") != "K" || os.Getenv("BINANCE_API_SECRET") != "S" {
		t.Fatal("the key did not reach the environment")
	}
}

func TestRefusalsAreReportedWithoutRetrying(t *testing.T) {
	code, hits := control(t)
	t.Setenv("CONTROL_TOKEN", "wrong")
	if why := FromControl(5, 0); !strings.Contains(why, "401") || !strings.Contains(why, "nope") {
		t.Fatal(why)
	}
	if hits.Load() != 1 {
		t.Fatalf("%d hits, want 1", hits.Load())
	}
	t.Setenv("CONTROL_TOKEN", "tok")
	code.Store(404)
	if why := FromControl(5, 0); !strings.Contains(why, "404") {
		t.Fatal(why)
	}
}

func TestAServerErrorIsRetried(t *testing.T) {
	code, hits := control(t)
	code.Store(503)
	if why := FromControl(3, 0); !strings.Contains(why, "503") || hits.Load() != 3 {
		t.Fatalf("%q after %d hits", why, hits.Load())
	}
}

func TestUnreachableControlIsRetriedThenReported(t *testing.T) {
	t.Setenv("BINANCE_API_KEY", "")
	t.Setenv("KEY_FROM_CONTROL", "http://127.0.0.1:9") // nothing listens there
	t.Setenv("CONTROL_TOKEN", "tok")
	if why := FromControl(2, 0); !strings.Contains(why, "not reachable") {
		t.Fatal(why)
	}
}

func TestOffOrOverridden(t *testing.T) {
	t.Setenv("KEY_FROM_CONTROL", "")
	if FromControl(12, 0) != "" {
		t.Fatal("off must be silent")
	}
	t.Setenv("KEY_FROM_CONTROL", "http://127.0.0.1:9")
	t.Setenv("BINANCE_API_KEY", "set-on-the-runner")
	if FromControl(12, 0) != "" { // an explicit key wins
		t.Fatal("an explicit key must win")
	}
	t.Setenv("BINANCE_API_KEY", "")
	t.Setenv("CONTROL_TOKEN", "")
	if why := FromControl(1, 0); why != "KEY_FROM_CONTROL needs CONTROL_TOKEN" {
		t.Fatal(why)
	}
}
