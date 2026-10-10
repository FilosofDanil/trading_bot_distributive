// Package store persists per-chat state (keys, risk window, last snapshot) in a JSON file.
// API secrets are encrypted at rest with AES-256-GCM.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Wizard steps for the /start key dialog.
const (
	StepNone        = ""
	StepAwaitKey    = "await_key"
	StepAwaitSecret = "await_secret"
)

// Snapshot is the result of the latest 10-second check, saved on every run.
type Snapshot struct {
	CheckedAt     time.Time `json:"checked_at"`
	WalletBalance float64   `json:"wallet_balance"`
	UnrealizedPnL float64   `json:"unrealized_pnl"`
	Equity        float64   `json:"equity"`
	LossAmount    float64   `json:"loss_amount"`
	LossPct       float64   `json:"loss_pct"`
	OpenPositions int       `json:"open_positions"`
	Error         string    `json:"error,omitempty"`
}

type User struct {
	ChatID int64  `json:"chat_id"`
	Lang   string `json:"lang"`
	Step   string `json:"step,omitempty"`

	APIKeyEnc    string `json:"api_key_enc,omitempty"`
	APISecretEnc string `json:"api_secret_enc,omitempty"`
	PendingKey   string `json:"pending_key_enc,omitempty"` // API key received, waiting for the secret
	KeyHash      string `json:"key_hash,omitempty"`        // kept after /stop to recognise the same key

	Active bool `json:"active"`

	// Risk window: losses are measured against Baseline (wallet balance at PeriodStart).
	PeriodStart time.Time `json:"period_start"`
	Baseline    float64   `json:"baseline"`

	Alerted   bool      `json:"alerted"`
	AlertedAt time.Time `json:"alerted_at"`

	Blocked      bool      `json:"blocked"`
	BlockedAt    time.Time `json:"blocked_at"`
	BlockedUntil time.Time `json:"blocked_until"`

	Last Snapshot `json:"last"`
}

type Store struct {
	mu    sync.Mutex
	path  string
	aead  cipher.AEAD
	users map[int64]*User
}

// Open loads the state file (creating its directory). If key is nil a random key is
// generated once and kept in secret.key next to the state file.
func Open(path string, key []byte) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if key == nil {
		var err error
		if key, err = loadOrCreateKey(filepath.Join(filepath.Dir(path), "secret.key")); err != nil {
			return nil, err
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, aead: aead, users: map[int64]*User{}}

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	var list []*User
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, u := range list {
		s.users[u.ChatID] = u
	}
	return s, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("%s is corrupt", path)
		}
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// Get returns a copy of the user's state (zero value + ok=false if unknown).
func (s *Store) Get(chatID int64) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[chatID]
	if !ok {
		return User{ChatID: chatID}, false
	}
	return *u, true
}

// Update applies fn to the user's state under the lock and persists the file.
func (s *Store) Update(chatID int64, fn func(u *User)) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[chatID]
	if !ok {
		u = &User{ChatID: chatID}
		s.users[chatID] = u
	}
	fn(u)
	return *u, s.saveLocked()
}

// Active lists the chat IDs that have keys and monitoring enabled.
func (s *Store) Active() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for id, u := range s.users {
		if u.Active {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *Store) saveLocked() error {
	list := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ChatID < list[j].ChatID })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Encrypt seals a secret for storage.
func (s *Store) Encrypt(plain string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// Decrypt opens a value produced by Encrypt.
func (s *Store) Decrypt(enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	n := s.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("ciphertext too short")
	}
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("cannot decrypt stored key (wrong ENCRYPTION_KEY?)")
	}
	return string(plain), nil
}

// Credentials decrypts the user's API key pair.
func (s *Store) Credentials(u User) (key, secret string, err error) {
	if u.APIKeyEnc == "" || u.APISecretEnc == "" {
		return "", "", errors.New("no API keys stored")
	}
	if key, err = s.Decrypt(u.APIKeyEnc); err != nil {
		return "", "", err
	}
	if secret, err = s.Decrypt(u.APISecretEnc); err != nil {
		return "", "", err
	}
	return key, secret, nil
}
