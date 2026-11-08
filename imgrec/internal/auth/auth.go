// Package auth: clients, their tokens, and which pictures each may see.
//
// A token is shown once, when the client is made; the db keeps only its
// sha256. A client sees the pictures it submitted (client_images, which
// outlives the jobs: jobs rows are deleted once answered). An admin sees
// everything. A picture two clients submitted is visible to both and is
// still answered once -- answers are keyed by content, not by who asked.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const schema = `
CREATE TABLE IF NOT EXISTS clients (
  name      TEXT    PRIMARY KEY,
  token_sha TEXT    NOT NULL UNIQUE,
  admin     INTEGER NOT NULL DEFAULT 0,
  created   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE TABLE IF NOT EXISTS client_images (
  client    TEXT NOT NULL REFERENCES clients(name) ON DELETE CASCADE,
  image_sha TEXT NOT NULL,
  created   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
  PRIMARY KEY (client, image_sha)
);`

type Client struct {
	Name    string
	Admin   bool
	Created string
}

type Auth struct{ db *sql.DB }

func Open(db *sql.DB) (*Auth, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Auth{db}, nil
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func hash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Add makes a client and returns its token -- the only time it's shown.
func (a *Auth) Add(name string, admin bool) (string, error) {
	if !validName.MatchString(name) {
		return "", fmt.Errorf("a client name is letters, digits, '.', '_' or '-' (at most 64)")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := "imgrec_" + hex.EncodeToString(b)
	_, err := a.db.Exec("INSERT INTO clients(name, token_sha, admin) VALUES (?, ?, ?)", name, hash(token), admin)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return "", fmt.Errorf("client %q exists (remove it first to issue a new token)", name)
	}
	return token, err
}

// Remove deletes a client and what it was waiting for. Its answers stay:
// they belong to the pictures, not to whoever asked.
func (a *Auth) Remove(name string) error {
	r, err := a.db.Exec("DELETE FROM clients WHERE name = ?", name)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("no client %q", name)
	}
	a.db.Exec("DELETE FROM client_images WHERE client = ?", name)
	_, err = a.db.Exec("DELETE FROM jobs WHERE client = ?", name)
	return err
}

func (a *Auth) List() ([]Client, error) {
	rows, err := a.db.Query("SELECT name, admin, created FROM clients ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Client
	for rows.Next() {
		var c Client
		if err := rows.Scan(&c.Name, &c.Admin, &c.Created); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

var ErrUnauthorized = errors.New("unauthorized")

// Authenticate is a bearer token -> its client.
func (a *Auth) Authenticate(token string) (Client, error) {
	if !strings.HasPrefix(token, "imgrec_") {
		return Client{}, ErrUnauthorized
	}
	want := hash(token)
	var c Client
	var got string
	err := a.db.QueryRow("SELECT name, admin, created, token_sha FROM clients WHERE token_sha = ?", want).
		Scan(&c.Name, &c.Admin, &c.Created, &got)
	if err != nil || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return Client{}, ErrUnauthorized
	}
	return c, nil
}

// Own records that a client submitted a picture; true if it's new to them.
func (a *Auth) Own(client, sha string) (bool, error) {
	r, err := a.db.Exec("INSERT OR IGNORE INTO client_images(client, image_sha) VALUES (?, ?)", client, sha)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// Sees: may this client see this picture?
func (a *Auth) Sees(c Client, sha string) (bool, error) {
	if c.Admin {
		return true, nil
	}
	var n int
	err := a.db.QueryRow("SELECT COUNT(*) FROM client_images WHERE client = ? AND image_sha = ?", c.Name, sha).Scan(&n)
	return n > 0, err
}
