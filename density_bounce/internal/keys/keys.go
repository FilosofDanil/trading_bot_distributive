// Package keys reads the Binance key from control (control/internal/keyserver): the owner stores it once with
// /start in Telegram, and a live runner reads it at start into its own environment (memory only, never written or
// logged). KEY_FROM_CONTROL=http://control.railway.internal:8090 turns it on; BINANCE_API_KEY set on the runner wins.
package keys

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// FromControl fills BINANCE_API_KEY / BINANCE_API_SECRET from control. It returns "" on success or when it is not
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
	client := http.Client{Timeout: 10 * time.Second}
	why := ""
	for attempt := 0; attempt < tries; attempt++ {
		req, err := http.NewRequest("GET", url+"/v1/binance-key", nil)
		if err != nil {
			return fmt.Sprintf("control not reachable (%v)", err)
		}
		req.Header.Set("X-Control-Token", token)
		resp, err := client.Do(req)
		if err != nil { // control restarting: try again
			why = fmt.Sprintf("control not reachable (%s: %v)", strings.TrimPrefix(fmt.Sprintf("%T", err), "*"), err)
		} else {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				var body struct {
					APIKey    string `json:"api_key"`
					APISecret string `json:"api_secret"`
				}
				if err := json.Unmarshal(raw, &body); err != nil {
					return fmt.Sprintf("control sent an unreadable key answer (%v)", err)
				}
				os.Setenv("BINANCE_API_KEY", body.APIKey)
				os.Setenv("BINANCE_API_SECRET", body.APISecret)
				return ""
			}
			var eb struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(raw, &eb) == nil {
				why = fmt.Sprintf("control answered %d: %s", resp.StatusCode, eb.Error)
			} else {
				why = fmt.Sprintf("control answered %d", resp.StatusCode)
			}
			if resp.StatusCode == 401 || resp.StatusCode == 404 || resp.StatusCode == 409 { // retrying will not change these
				return why
			}
		}
		if attempt < tries-1 {
			time.Sleep(wait)
		}
	}
	return why
}
