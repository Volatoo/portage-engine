package persistence

import (
	"net/url"
	"testing"

	"github.com/slchris/portage-engine/pkg/config"
)

func TestConnectionStringEscapesSplitCredentials(t *testing.T) {
	dsn, err := ConnectionString(config.DatabaseConfig{
		Host:     "postgres.internal",
		Port:     5432,
		Name:     "portage engine",
		User:     "build:user",
		Password: "p@ss:/?#[]",
		SSLMode:  "verify-full",
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.User.Username() != "build:user" {
		t.Fatalf("username = %q", parsed.User.Username())
	}
	password, ok := parsed.User.Password()
	if !ok || password != "p@ss:/?#[]" {
		t.Fatalf("password = %q, present=%v", password, ok)
	}
	if parsed.Path != "/portage engine" || parsed.Query().Get("sslmode") != "verify-full" {
		t.Fatalf("unexpected DSN fields: %s", dsn)
	}
}

func TestConnectionStringURLWins(t *testing.T) {
	const explicit = "postgres://explicit:secret@db.example/portage?sslmode=require"
	dsn, err := ConnectionString(config.DatabaseConfig{
		URL:  explicit,
		Host: "ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dsn != explicit {
		t.Fatalf("dsn = %q", dsn)
	}
}

func TestConnectionStringIPv6Host(t *testing.T) {
	dsn, err := ConnectionString(config.DatabaseConfig{
		Host:    "::1",
		Port:    5432,
		Name:    "portage",
		User:    "portage",
		SSLMode: "disable",
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "[::1]:5432" {
		t.Fatalf("host = %q", parsed.Host)
	}
}
