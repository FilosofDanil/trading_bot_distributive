package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPersistAndEncrypt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := s.Encrypt("my-secret-value")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := s.Update(42, func(u *User) {
		u.APIKeyEnc, u.APISecretEnc = enc, enc
		u.Active = true
		u.Last = Snapshot{CheckedAt: now, WalletBalance: 100, Equity: 90, LossPct: 10}
	}); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "my-secret-value") {
		t.Fatal("secret stored in clear text")
	}

	s2, err := Open(path, nil) // same auto-generated key from secret.key
	if err != nil {
		t.Fatal(err)
	}
	u, ok := s2.Get(42)
	if !ok || !u.Active || u.Last.LossPct != 10 || !u.Last.CheckedAt.Equal(now) {
		t.Fatalf("reloaded state: %+v", u)
	}
	k, sec, err := s2.Credentials(u)
	if err != nil || k != "my-secret-value" || sec != "my-secret-value" {
		t.Fatalf("credentials: %q %q %v", k, sec, err)
	}
	if ids := s2.Active(); len(ids) != 1 || ids[0] != 42 {
		t.Fatalf("active: %v", ids)
	}

	wrong := make([]byte, 32)
	s3, err := Open(path, wrong)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s3.Credentials(u); err == nil {
		t.Fatal("wrong key must not decrypt")
	}
}
