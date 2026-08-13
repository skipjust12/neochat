package db

import (
	"net/url"
	"testing"
)

// TestPostgresDSN_EscapesCredentials is the regression test for audit.md's
// DSN finding: a password containing URL-significant characters must not
// be able to change which host/database the connection actually points
// at. The interpolated version this replaced turned "p@ss/word" into a
// connection aimed at host "ss".
func TestPostgresDSN_EscapesCredentials(t *testing.T) {
	dsn := postgresDSN("neochat", "p@ss/word#1", "db.internal", "5432", "neochat", "require")

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("built DSN does not parse: %v (dsn=%q)", err, dsn)
	}
	if parsed.Host != "db.internal:5432" {
		t.Errorf("Host = %q, want db.internal:5432 -- the password must not bleed into the host", parsed.Host)
	}
	if parsed.Path != "/neochat" {
		t.Errorf("Path = %q, want /neochat", parsed.Path)
	}
	gotPassword, _ := parsed.User.Password()
	if gotPassword != "p@ss/word#1" {
		t.Errorf("password round-tripped as %q, want %q", gotPassword, "p@ss/word#1")
	}
	if parsed.User.Username() != "neochat" {
		t.Errorf("username = %q, want neochat", parsed.User.Username())
	}
}

func TestPostgresDSN_CarriesSSLMode(t *testing.T) {
	for _, sslmode := range []string{"disable", "require", "verify-full"} {
		dsn := postgresDSN("u", "p", "127.0.0.1", "5432", "d", sslmode)
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("built DSN does not parse: %v", err)
		}
		if got := parsed.Query().Get("sslmode"); got != sslmode {
			t.Errorf("sslmode = %q, want %q (dsn=%q)", got, sslmode, dsn)
		}
	}
}

// TestPostgresDSN_IPv6Host checks the host is bracketed, which a bare
// "host:port" concatenation would not do.
func TestPostgresDSN_IPv6Host(t *testing.T) {
	dsn := postgresDSN("u", "p", "::1", "5432", "d", "disable")
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("built DSN does not parse: %v (dsn=%q)", err, dsn)
	}
	if parsed.Hostname() != "::1" || parsed.Port() != "5432" {
		t.Errorf("host/port = %q/%q, want ::1/5432 (dsn=%q)", parsed.Hostname(), parsed.Port(), dsn)
	}
}
