package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"

	"golang.org/x/crypto/bcrypt"
)

// secretsFileName is the file, next to the database, that holds secrets
// hopperd generated itself because none were configured.
const secretsFileName = "secrets.json"

// storedSecrets is the on-disk format of secretsFileName.
type storedSecrets struct {
	JWTSecret         string `json:"jwt_secret,omitempty"`
	EncryptionKey     string `json:"encryption_key,omitempty"`
	AdminPasswordHash string `json:"admin_password_hash,omitempty"`
}

// SecretsPath is where generated secrets are stored: next to the
// database, i.e. inside the /config volume under Docker.
func (c *Config) SecretsPath() string {
	return filepath.Join(filepath.Dir(c.Database.Path), secretsFileName)
}

// ensureSecrets fills JWT secret, encryption key and admin password hash
// when they are still unset after the config file and HOPPER_*
// environment variables — so the Docker image runs with nothing but
// `docker compose up`. Values are taken from the secrets file if present,
// otherwise generated and written there (0600). An explicitly configured
// value always wins and is never written to the file.
//
// The encryption key protects stored hoster credentials: the secrets file
// must be kept (it lives in /config, next to the database) — losing it
// means re-entering accounts.
func (c *Config) ensureSecrets() error {
	if c.Server.JWTSecret != "" && c.Accounts.EncryptionKey != "" && c.Server.AdminPasswordHash != "" {
		return nil
	}
	stored, err := readSecrets(c.SecretsPath())
	if err != nil {
		return err
	}
	changed := false

	if c.Server.JWTSecret == "" {
		if stored.JWTSecret == "" {
			if stored.JWTSecret, err = randomString(48); err != nil {
				return err
			}
			changed = true
		}
		c.Server.JWTSecret = stored.JWTSecret
	}
	if c.Accounts.EncryptionKey == "" {
		if stored.EncryptionKey == "" {
			if stored.EncryptionKey, err = randomString(32); err != nil {
				return err
			}
			changed = true
		}
		c.Accounts.EncryptionKey = stored.EncryptionKey
	}
	if c.Server.AdminPasswordHash == "" {
		if stored.AdminPasswordHash == "" {
			pw, err := randomString(20)
			if err != nil {
				return err
			}
			hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
			if err != nil {
				return fmt.Errorf("hash generated admin password: %w", err)
			}
			stored.AdminPasswordHash = string(hash)
			c.GeneratedAdminPassword = pw
			changed = true
		}
		c.Server.AdminPasswordHash = stored.AdminPasswordHash
	}

	if changed {
		return writeSecrets(c.SecretsPath(), stored)
	}
	return nil
}

// SetAdminPassword stores a new admin password (as bcrypt hash) in the
// secrets file — hopperd -set-admin-password. It takes effect on the next
// start, and only if no admin_password_hash is configured explicitly
// (HOPPER_ADMIN_PASSWORD_HASH or hopper.yaml), which would take precedence.
func SetAdminPassword(secretsPath, password string) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	stored, err := readSecrets(secretsPath)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	stored.AdminPasswordHash = string(hash)
	return writeSecrets(secretsPath, stored)
}

func readSecrets(path string) (storedSecrets, error) {
	var s storedSecrets
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is derived from database.path (operator config)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

func writeSecrets(path string, s storedSecrets) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// randomString returns n characters from an unambiguous alphabet
// (no 0/O/1/l), drawn from crypto/rand.
func randomString(n int) (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate secret: %w", err)
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out), nil
}
