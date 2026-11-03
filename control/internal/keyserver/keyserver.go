// Package keyserver hands the Binance key the owner stored with /start to the trading runners, so the key
// lives in one place (control's encrypted state) and a new key reaches the runners on their next start.
//
// It is off unless KEY_SERVER_PORT is set, listens on the Railway private network only (the services have
// no public domain), answers only requests carrying X-Control-Token, and logs every request without the key.
//
//	GET /v1/binance-key -> 200 {"chat_id": 1, "api_key": "...", "api_secret": "..."}
//	                       401 wrong token · 404 no account with a stored key · 409 more than one
package keyserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"control/internal/store"
)

// Source is the part of store.Store the server reads.
type Source interface {
	Active() []int64
	Get(chatID int64) (store.User, bool)
	Credentials(u store.User) (key, secret string, err error)
}

// Handler serves the stored key of the single active account.
func Handler(src Source, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/binance-key", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Control-Token")
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			log.Printf("keyserver: refused %s (bad token)", r.RemoteAddr)
			reply(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		var users []store.User
		for _, id := range src.Active() {
			if u, ok := src.Get(id); ok && u.APIKeyEnc != "" && u.APISecretEnc != "" {
				users = append(users, u)
			}
		}
		switch len(users) {
		case 0:
			reply(w, http.StatusNotFound, map[string]string{"error": "no active account with a stored key: /start in Telegram"})
			return
		case 1:
		default:
			reply(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("%d accounts have keys; expected one", len(users))})
			return
		}
		key, secret, err := src.Credentials(users[0])
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("keyserver: key of chat %d served to %s", users[0].ChatID, r.RemoteAddr)
		reply(w, http.StatusOK, map[string]any{"chat_id": users[0].ChatID, "api_key": key, "api_secret": secret})
	})
	return mux
}

func reply(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// Serve listens on [::]:port (IPv6 + IPv4: Railway's private network is IPv6) until ctx ends.
func Serve(ctx context.Context, port int, h http.Handler) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", port))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
