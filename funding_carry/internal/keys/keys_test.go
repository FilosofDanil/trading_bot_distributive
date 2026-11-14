package keys

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type ctl struct {
	code int
	hits int
}

func control(t *testing.T) *ctl {
	c := &ctl{code: 200}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits++
		ok := r.URL.Path == "/v1/binance-key" && r.Header.Get("X-Control-Token") == "tok"
		code := c.code
		if !ok {
			code = 401
		}
		var body any = map[string]any{"error": "nope"}
		if code == 200 {
			body = map[string]any{"api_key": "K", "api_secret": "S", "chat_id": 1}
		}
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("BINANCE_API_KEY", "")
	os.Unsetenv("BINANCE_API_KEY")
	t.Setenv("BINANCE_API_SECRET", "")
	os.Unsetenv("BINANCE_API_SECRET")
	t.Setenv("KEY_FROM_CONTROL", ts.URL+"/")
	t.Setenv("CONTROL_TOKEN", "tok")
	return c
}

func TestKeyLandsInTheEnvironment(t *testing.T) {
	control(t)
	if why := FromControl(1, 0); why != "" {
		t.Fatal(why)
	}
	if os.Getenv("BINANCE_API_KEY") != "K" || os.Getenv("BINANCE_API_SECRET") != "S" {
		t.Fatal("key not set")
	}
}

func TestRefusalsAreReportedWithoutRetrying(t *testing.T) {
	c := control(t)
	t.Setenv("CONTROL_TOKEN", "wrong")
	if why := FromControl(5, 0); !strings.Contains(why, "401") || c.hits != 1 {
		t.Fatal(why, c.hits)
	}
	t.Setenv("CONTROL_TOKEN", "tok")
	c.code = 404
	if why := FromControl(5, 0); !strings.Contains(why, "404") {
		t.Fatal(why)
	}
}

func TestUnreachableControlIsRetriedThenReported(t *testing.T) {
	t.Setenv("BINANCE_API_KEY", "")
	os.Unsetenv("BINANCE_API_KEY")
	t.Setenv("KEY_FROM_CONTROL", "http://127.0.0.1:9") // nothing listens there
	t.Setenv("CONTROL_TOKEN", "tok")
	if why := FromControl(2, 0); !strings.Contains(why, "not reachable") {
		t.Fatal(why)
	}
}

func TestOffOrOverridden(t *testing.T) {
	os.Unsetenv("KEY_FROM_CONTROL")
	t.Setenv("KEY_FROM_CONTROL", "")
	if FromControl(12, 0) != "" {
		t.Fatal()
	}
	t.Setenv("KEY_FROM_CONTROL", "http://127.0.0.1:9")
	t.Setenv("BINANCE_API_KEY", "set-on-the-runner")
	if FromControl(12, 0) != "" { // an explicit key wins
		t.Fatal()
	}
}
