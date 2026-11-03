package keyserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"control/internal/store"
)

type fake map[int64]store.User

func (f fake) Active() []int64 {
	var ids []int64
	for id, u := range f {
		if u.Active {
			ids = append(ids, id)
		}
	}
	return ids
}

func (f fake) Get(id int64) (store.User, bool) { u, ok := f[id]; return u, ok }

func (f fake) Credentials(u store.User) (string, string, error) {
	if u.APIKeyEnc == "bad" {
		return "", "", errors.New("cannot decrypt")
	}
	return "key-" + u.APIKeyEnc, "secret-" + u.APISecretEnc, nil
}

func get(t *testing.T, h http.Handler, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/binance-key", nil)
	if token != "" {
		req.Header.Set("X-Control-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestServesTheSingleActiveKey(t *testing.T) {
	src := fake{7: {ChatID: 7, Active: true, APIKeyEnc: "a", APISecretEnc: "b"},
		8: {ChatID: 8, Active: false, APIKeyEnc: "c", APISecretEnc: "d"}}
	code, body := get(t, Handler(src, "tok"), "tok")
	if code != 200 || body["api_key"] != "key-a" || body["api_secret"] != "secret-b" || body["chat_id"] != float64(7) {
		t.Fatalf("got %d %v", code, body)
	}
}

func TestRefusals(t *testing.T) {
	one := fake{7: {ChatID: 7, Active: true, APIKeyEnc: "a", APISecretEnc: "b"}}
	if code, body := get(t, Handler(one, "tok"), "wrong"); code != 401 || body["api_key"] != nil {
		t.Errorf("wrong token: %d %v", code, body)
	}
	if code, _ := get(t, Handler(one, "tok"), ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := get(t, Handler(one, ""), ""); code != 401 {
		t.Errorf("server without a token must refuse everyone: %d", code)
	}
	if code, _ := get(t, Handler(fake{7: {ChatID: 7, Active: true}}, "tok"), "tok"); code != 404 {
		t.Errorf("no stored key: %d", code)
	}
	two := fake{7: {ChatID: 7, Active: true, APIKeyEnc: "a", APISecretEnc: "b"},
		8: {ChatID: 8, Active: true, APIKeyEnc: "c", APISecretEnc: "d"}}
	if code, _ := get(t, Handler(two, "tok"), "tok"); code != 409 {
		t.Errorf("two accounts: %d", code)
	}
	if code, _ := get(t, Handler(fake{7: {ChatID: 7, Active: true, APIKeyEnc: "bad", APISecretEnc: "b"}}, "tok"), "tok"); code != 500 {
		t.Errorf("undecryptable: %d", code)
	}
}
