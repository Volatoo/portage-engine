package persistence

import (
	"math"
	"net/url"
	"strconv"
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

func TestCheckedPoolSizeRejectsOverflow(t *testing.T) {
	if strconv.IntSize <= 32 {
		t.Skip("int cannot represent a value above int32 on this platform")
	}
	if _, err := checkedPoolSize("maximum", int64ToInt(t, math.MaxInt32+1)); err == nil {
		t.Fatal("checkedPoolSize accepted a value above int32")
	}
	if got, err := checkedPoolSize("minimum", 12); err != nil || got != 12 {
		t.Fatalf("checkedPoolSize(12) = %d, %v", got, err)
	}
}

func int64ToInt(t *testing.T, value int64) int {
	t.Helper()
	converted := int(value)
	if int64(converted) != value {
		t.Fatalf("test value %d does not fit int", value)
	}
	return converted
}
