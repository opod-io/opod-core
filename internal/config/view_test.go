package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// The view prints no credential: the admin and join tokens are not in it, a
// vLLM key is redacted, and a DSN's password is replaced (a Postgres DSN in
// clear was the one secret `opod config show` still printed).
func TestTheViewPrintsNoCredential(t *testing.T) {
	for dsn, want := range map[string]string{
		"postgres://opod:s3cret-placeholder@db:5432/opod?sslmode=require": "postgres://opod:redacted@db:5432/opod?sslmode=require",
		"host=db user=opod password=s3cret-placeholder dbname=opod":       "host=db user=opod password=redacted dbname=opod",
		"host=db password='s3 cret' dbname=opod":                          "host=db password=redacted dbname=opod",
		"/home/me/.opod/state.db":                                         "/home/me/.opod/state.db",
		"postgres://opod@db/opod":                                         "postgres://opod@db/opod",
	} {
		if got := RedactDSN(dsn); got != want {
			t.Errorf("%q → %q, want %q", dsn, got, want)
		}
	}
	c := Default()
	c.Auth.AdminToken, c.Auth.JoinToken = "sk-orc-placeholder-admin-01", "sk-orc-placeholder-join-01"
	c.Storage.DSN = "postgres://opod:s3cret-placeholder@db/opod"
	b, _ := json.Marshal(ViewOf(c))
	for _, secret := range []string{"placeholder-admin", "placeholder-join", "s3cret"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the view carries %q:\n%s", secret, b)
		}
	}
}
