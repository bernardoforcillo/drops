package pgwire

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// config is a parsed connection string.
type config struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
	SSLMode  string
	// Options carries the remaining key=value pairs, passed through in
	// the startup message (search_path, application_name, ...).
	Options map[string]string
}

// parseDSN accepts both spellings PostgreSQL clients take: the URL
// form (postgres://user:pass@host:5432/db?sslmode=disable) and the
// keyword form (host=localhost user=drops dbname=drops).
//
// Anything absent falls back to libpq's environment variables and then
// to libpq's defaults, so a DSN of "" against a local trust-auth
// server with PGDATABASE set is a valid thing to hand this function.
func parseDSN(dsn string) (*config, error) {
	cfg := &config{Options: map[string]string{}}
	trimmed := strings.TrimSpace(dsn)
	var err error
	switch {
	case trimmed == "":
		// Environment only.
	case strings.HasPrefix(trimmed, "postgres://"), strings.HasPrefix(trimmed, "postgresql://"):
		err = parseURLDSN(trimmed, cfg)
	case strings.ContainsRune(trimmed, '='):
		err = parseKeywordDSN(trimmed, cfg)
	default:
		return nil, fmt.Errorf("connection string %q is neither a postgres:// URL nor a list of key=value pairs", dsn)
	}
	if err != nil {
		return nil, err
	}
	applyEnvDefaults(cfg)
	if cfg.User == "" {
		return nil, fmt.Errorf("connection string %q names no user, and neither PGUSER nor USER is set — add one as postgres://USER@host/db", dsn)
	}
	if cfg.Database == "" {
		// libpq's default: the database is named after the user.
		cfg.Database = cfg.User
	}
	return cfg, nil
}

func parseURLDSN(dsn string, cfg *config) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse connection URL: %w", err)
	}
	if u.User != nil {
		cfg.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cfg.Password = pw
		}
	}
	if h := u.Hostname(); h != "" {
		cfg.Host = h
	}
	if p := u.Port(); p != "" {
		cfg.Port = p
	}
	cfg.Database = strings.TrimPrefix(u.Path, "/")
	for k, vs := range u.Query() {
		if len(vs) == 0 {
			continue
		}
		assignParam(cfg, k, vs[0])
	}
	return nil
}

// parseKeywordDSN reads the "host=x user=y" form. Values may be single
// quoted, with backslash escapes inside the quotes, as libpq allows.
func parseKeywordDSN(dsn string, cfg *config) error {
	rest := dsn
	for {
		rest = strings.TrimLeft(rest, " \t\n")
		if rest == "" {
			return nil
		}
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			return fmt.Errorf("connection string fragment %q has no '='", rest)
		}
		key := strings.TrimSpace(rest[:eq])
		rest = strings.TrimLeft(rest[eq+1:], " \t")
		var value string
		if strings.HasPrefix(rest, "'") {
			var b strings.Builder
			i := 1
			for ; i < len(rest) && rest[i] != '\''; i++ {
				if rest[i] == '\\' && i+1 < len(rest) {
					i++
				}
				b.WriteByte(rest[i])
			}
			if i >= len(rest) {
				return fmt.Errorf("connection string has an unterminated quoted value for %q", key)
			}
			value, rest = b.String(), rest[i+1:]
		} else {
			end := strings.IndexAny(rest, " \t\n")
			if end < 0 {
				end = len(rest)
			}
			value, rest = rest[:end], rest[end:]
		}
		assignParam(cfg, key, value)
	}
}

func assignParam(cfg *config, key, value string) {
	switch key {
	case "host":
		cfg.Host = value
	case "port":
		cfg.Port = value
	case "user":
		cfg.User = value
	case "password":
		cfg.Password = value
	case "dbname", "database":
		cfg.Database = value
	case "sslmode":
		cfg.SSLMode = value
	case "":
	default:
		cfg.Options[key] = value
	}
}

// applyEnvDefaults fills the blanks from libpq's environment variables
// and then from its documented defaults.
func applyEnvDefaults(cfg *config) {
	if cfg.Host == "" {
		cfg.Host = firstNonEmpty(os.Getenv("PGHOST"), "localhost")
	}
	if cfg.Port == "" {
		cfg.Port = firstNonEmpty(os.Getenv("PGPORT"), "5432")
	}
	if cfg.User == "" {
		cfg.User = firstNonEmpty(os.Getenv("PGUSER"), os.Getenv("USER"))
	}
	if cfg.Password == "" {
		cfg.Password = os.Getenv("PGPASSWORD")
	}
	if cfg.Database == "" {
		cfg.Database = os.Getenv("PGDATABASE")
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = firstNonEmpty(os.Getenv("PGSSLMODE"), "prefer")
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// address renders the TCP address, or the socket path when Host names
// a directory in the Unix-socket convention libpq uses.
func (c *config) address() (network, address string) {
	if strings.HasPrefix(c.Host, "/") {
		return "unix", c.Host + "/.s.PGSQL." + c.Port
	}
	return "tcp", net.JoinHostPort(c.Host, c.Port)
}
