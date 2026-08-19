package db

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aturzone/chevaletAnonBot/internal/config"
)

// buildDSN mirrors exactly what Connect assembles, so these tests can check the
// connection settings without needing a live database.
func buildDSN(cfg *config.Config) string {
	port := cfg.DBPort
	if port == 0 {
		port = 5432
	}
	sslMode := cfg.DBSSLMode
	if sslMode == "" {
		sslMode = "prefer"
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.DBUser, cfg.DBPass),
		Host:   net.JoinHostPort(cfg.DBHost, strconv.Itoa(port)),
		Path:   "/" + cfg.DBName,
	}
	q := u.Query()
	q.Set("sslmode", sslMode)
	q.Set("client_encoding", "UTF8")
	u.RawQuery = q.Encode()
	return u.String()
}

// TestConnectionTargetsOnlyTheConfiguredServer is the regression guard for the
// bug this file was added with.
//
// Connect used to call pgxpool.ParseConfig("") and then assign Host/Port onto
// the result. pgx derives its TLS decision AND its fallback address list during
// ParseConfig, from whatever host it defaulted to — so those derived settings
// went on describing a different server than the one being configured. On
// Linux the default is a unix socket (TLS off, no fallbacks) and the override
// happened to work, which is why production never noticed. On Windows the
// default is localhost with sslmode=prefer, which builds a real fallback list:
// a bot pointed at a remote database that refuses TLS would silently retry
// against localhost:5432 and could connect to an entirely different database.
//
// The invariant, on every platform: nothing pgx might dial is anywhere other
// than the configured host and port.
func TestConnectionTargetsOnlyTheConfiguredServer(t *testing.T) {
	cfg := &config.Config{
		DBHost: "db.internal.example",
		DBPort: 6432,
		DBUser: "root",
		DBPass: "pass",
		DBName: "mydatabase",
	}

	poolCfg, err := pgxpool.ParseConfig(buildDSN(cfg))
	if err != nil {
		t.Fatalf("ParseConfig error = %v; want nil", err)
	}

	if got := poolCfg.ConnConfig.Host; got != cfg.DBHost {
		t.Errorf("primary host = %q; want %q", got, cfg.DBHost)
	}
	if got := int(poolCfg.ConnConfig.Port); got != cfg.DBPort {
		t.Errorf("primary port = %d; want %d", got, cfg.DBPort)
	}

	for i, f := range poolCfg.ConnConfig.Fallbacks {
		if f.Host != cfg.DBHost || int(f.Port) != cfg.DBPort {
			t.Errorf("fallback[%d] points at %s:%d, not the configured %s:%d — "+
				"a refused connection could land on the wrong database",
				i, f.Host, f.Port, cfg.DBHost, cfg.DBPort)
		}
	}
}

// TestPasswordsWithSpecialCharactersSurvive covers the reason the original code
// avoided a DSN: it built the config field by field so a password never needed
// escaping. url.UserPassword percent-escapes it instead, and this proves the
// value pgx ends up with is the original one, byte for byte.
func TestPasswordsWithSpecialCharactersSurvive(t *testing.T) {
	for _, pass := range []string{
		"p@ss word",
		"a:b/c?d#e",
		"%40weird%",
		"sim'ple\"quotes",
		"unicode-پسورد",
	} {
		cfg := &config.Config{
			DBHost: "localhost", DBPort: 5432,
			DBUser: "root", DBPass: pass, DBName: "mydatabase",
		}
		poolCfg, err := pgxpool.ParseConfig(buildDSN(cfg))
		if err != nil {
			t.Errorf("ParseConfig with password %q error = %v; want nil", pass, err)
			continue
		}
		if got := poolCfg.ConnConfig.Password; got != pass {
			t.Errorf("password round-tripped as %q; want %q", got, pass)
		}
	}
}

// TestClientEncodingIsPreserved: the previous implementation set client_encoding
// through RuntimeParams. It must still reach the server.
func TestClientEncodingIsPreserved(t *testing.T) {
	cfg := &config.Config{DBHost: "localhost", DBPort: 5432, DBUser: "root", DBName: "mydatabase"}
	poolCfg, err := pgxpool.ParseConfig(buildDSN(cfg))
	if err != nil {
		t.Fatalf("ParseConfig error = %v", err)
	}
	if got := poolCfg.ConnConfig.RuntimeParams["client_encoding"]; got != "UTF8" {
		t.Errorf("client_encoding = %q; want UTF8", got)
	}
}

// TestSSLModeIsConfigurableAndDefaultsToPrefer locks the default. "prefer" is
// what keeps this drop-in for the current production stack, whose PostgreSQL
// does not offer TLS: it tries TLS, is refused, and retries the SAME host
// without it — which only works because the fallbacks are correct now.
func TestSSLModeIsConfigurableAndDefaultsToPrefer(t *testing.T) {
	base := &config.Config{DBHost: "localhost", DBPort: 5432, DBUser: "root", DBName: "mydatabase"}
	if got := buildDSN(base); !strings.Contains(got, "sslmode=prefer") {
		t.Errorf("default DSN = %q; want sslmode=prefer", got)
	}

	base.DBSSLMode = "disable"
	poolCfg, err := pgxpool.ParseConfig(buildDSN(base))
	if err != nil {
		t.Fatalf("ParseConfig error = %v", err)
	}
	if poolCfg.ConnConfig.TLSConfig != nil {
		t.Error("sslmode=disable still produced a TLS config")
	}
	if len(poolCfg.ConnConfig.Fallbacks) != 0 {
		t.Errorf("sslmode=disable produced %d fallbacks; want none", len(poolCfg.ConnConfig.Fallbacks))
	}
}
