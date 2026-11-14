// Package keys gets the Binance key from control (control/internal/keyserver): the owner stores it once with
// /start in Telegram, and a live runner reads it at start into its own environment (memory only, never written
// or logged). KEY_FROM_CONTROL=http://control.railway.internal:8090 turns it on; BINANCE_API_KEY set on the
// runner wins. (carry/keys.py)
package keys

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Sleep is replaced in tests.
var Sleep = time.Sleep

// FromControl fills BINANCE_API_KEY / BINANCE_API_SECRET from control. It returns "" on success or when not
// configured, otherwise why the key could not be read (the runner then starts live-blocked).
func FromControl(tries int, wait time.Duration) string {
	url := strings.TrimRight(strings.TrimSpace(os.Getenv("KEY_FROM_CONTROL")), "/")
	if url == "" || strings.TrimSpace(os.Getenv("BINANCE_API_KEY")) != "" {
		return ""
	}
	token := strings.TrimSpace(os.Getenv("CONTROL_TOKEN"))
	if token == "" {
		return "KEY_FROM_CONTROL needs CONTROL_TOKEN"
	}
	client := &http.Client{Timeout: 10 * time.Second}
	why := ""
	for attempt := 0; attempt < tries; attempt++ {
		req, err := http.NewRequest("GET", url+"/v1/binance-key", nil)
		if err == nil {
			req.Header.Set("X-Control-Token", token)
			var r *http.Response
			if r, err = client.Do(req); err == nil {
				var body map[string]any
				derr := json.NewDecoder(r.Body).Decode(&body)
				r.Body.Close()
				if r.StatusCode == 200 {
					k, ok1 := body["api_key"].(string)
					s, ok2 := body["api_secret"].(string)
					if derr != nil || !ok1 || !ok2 {
						return "control sent an unreadable key"
					}
					os.Setenv("BINANCE_API_KEY", k)
					os.Setenv("BINANCE_API_SECRET", s)
					return ""
				}
				if derr == nil {
					msg, _ := body["error"].(string)
					why = fmt.Sprintf("control answered %d: %s", r.StatusCode, msg)
				} else {
					why = fmt.Sprintf("control answered %d", r.StatusCode)
				}
				if r.StatusCode == 401 || r.StatusCode == 404 || r.StatusCode == 409 { // retrying will not change these
					return why
				}
			}
		}
		if err != nil { // control restarting: try again
			why = fmt.Sprintf("control not reachable (TransportError: %v)", err)
		}
		if attempt < tries-1 {
			Sleep(wait)
		}
	}
	return why
}
