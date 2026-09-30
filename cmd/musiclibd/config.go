package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	apihttp "musiclib/internal/http"
)

// Environment variables (DESIGN.md §11.1: configuration only from the
// environment). There are no flags and no configuration file.
const (
	envDatabaseURL  = "DATABASE_URL"
	envPublicOrigin = "PUBLIC_ORIGIN"
	envHTTPAddr     = "HTTP_ADDR"
	envWorkers      = "WORKERS"
	envPassword     = "MUSICLIB_PASSWORD"

	defaultHTTPAddr = ":8080"
	minWorkers      = 1
	maxWorkers      = 16
)

// codeConfig is the error code of an invalid configuration.
const codeConfig = "config_invalid"

// Config is the validated configuration of the server.
type Config struct {
	// DatabaseURL may contain the database password: it is never logged,
	// and LogValue leaves it out.
	DatabaseURL string
	// PublicOrigin is the origin the API accepts in Host and Origin
	// (§10.4, internal/http).
	PublicOrigin string
	HTTPAddr     string
	Workers      int
	// Password is the sign-in password, read by the server alone
	// (loadPassword), never by loadConfig, which the offline commands
	// share. It is never logged: LogValue leaves it out.
	Password string
}

// LogValue is what slog prints for a Config: everything but DatabaseURL
// and Password.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("http_addr", c.HTTPAddr),
		slog.String("public_origin", c.PublicOrigin),
		slog.Int("workers", c.Workers),
	)
}

// loadConfig reads and validates the configuration. cpus is the number of
// CPUs available to the process, for the WORKERS default (§6.1). Every
// problem is reported at once, so the operator fixes them in one round.
func loadConfig(getenv func(string) string, cpus int) (Config, error) {
	var errs []error
	cfg := Config{
		DatabaseURL: getenv(envDatabaseURL),
		HTTPAddr:    getenv(envHTTPAddr),
	}

	if cfg.DatabaseURL == "" {
		errs = append(errs, fmt.Errorf("%s is required", envDatabaseURL))
	} else if _, err := pgxpool.ParseConfig(cfg.DatabaseURL); err != nil {
		// The parse error is not included: pgx redacts passwords only on a
		// best-effort basis for malformed input (N-064).
		errs = append(errs, fmt.Errorf("%s is not a valid PostgreSQL connection string "+
			"(details withheld because they may contain the password)", envDatabaseURL))
	}

	origin, err := parseOrigin(getenv(envPublicOrigin))
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", envPublicOrigin, err))
	}
	cfg.PublicOrigin = origin

	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = defaultHTTPAddr
	}
	if err := checkHTTPAddr(cfg.HTTPAddr); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", envHTTPAddr, err))
	}

	workers, err := parseWorkers(getenv(envWorkers), cpus)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", envWorkers, err))
	}
	cfg.Workers = workers

	if len(errs) > 0 {
		return Config{}, &bootError{code: codeConfig, msg: "invalid configuration", err: errors.Join(errs...)}
	}
	return cfg, nil
}

// loadPassword reads the sign-in password of the server. A missing or
// unusable one stops the boot with password_invalid; the message names the
// rule and where to set the value, never the value or its length.
func loadPassword(getenv func(string) string) (string, error) {
	p := getenv(envPassword)
	if err := apihttp.CheckPassword(p); err != nil {
		return "", &bootError{code: codePassword, msg: envPassword + " " + err.Error() +
			": set it in .env next to compose.yaml, for example to the output of `openssl rand -base64 24`, " +
			"then run `docker compose up -d --wait`"}
	}
	return p, nil
}

// defaultWorkers is max(1, min(4, available CPUs)) (§6.1).
func defaultWorkers(cpus int) int {
	return max(1, min(4, cpus))
}

// parseWorkers accepts a plain decimal integer in 1..16 (§6.1); empty means
// the default. Signs, leading zeros and spaces are rejected.
func parseWorkers(s string, cpus int) (int, error) {
	if s == "" {
		return defaultWorkers(cpus), nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || strconv.Itoa(n) != s {
		return 0, fmt.Errorf("%q is not a plain decimal integer", s)
	}
	if n < minWorkers || n > maxWorkers {
		return 0, fmt.Errorf("%d is outside %d..%d", n, minWorkers, maxWorkers)
	}
	return n, nil
}

// checkHTTPAddr accepts host:port with a decimal port in 1..65535; the host
// may be empty (all interfaces).
func checkHTTPAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || strconv.Itoa(n) != port || n < 1 || n > 65535 {
		return fmt.Errorf("port %q is not a number in 1..65535", port)
	}
	return nil
}

// parseOrigin accepts exactly the serialization of an HTTP origin that
// browsers send in the Origin header (§10.4): scheme "http" or "https",
// lowercase ASCII host, optional non-default port, and nothing else, not
// even a trailing slash. A non-canonical spelling is refused rather than
// normalized, so that the value the API compares is the one configured.
func parseOrigin(s string) (string, error) {
	if s == "" {
		return "", errors.New("required, for example http://127.0.0.1:8080")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%q is not a URL", s)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("%q: the scheme must be http or https", s)
	case u.Opaque != "" || u.User != nil:
		return "", fmt.Errorf("%q: an origin has no user information", s)
	case u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", fmt.Errorf("%q: an origin has no path, query or fragment, not even a trailing slash", s)
	case u.Hostname() == "":
		return "", fmt.Errorf("%q: the host is missing", s)
	}
	host := u.Hostname()
	for _, r := range host {
		if r > 0x7f || ('A' <= r && r <= 'Z') {
			return "", fmt.Errorf("%q: the host must be lowercase ASCII (punycode for international names)", s)
		}
	}
	if port := u.Port(); port != "" || strings.HasSuffix(u.Host, ":") {
		n, err := strconv.Atoi(port)
		if err != nil || strconv.Itoa(n) != port || n < 1 || n > 65535 {
			return "", fmt.Errorf("%q: the port must be a number in 1..65535", s)
		}
		if (u.Scheme == "http" && n == 80) || (u.Scheme == "https" && n == 443) {
			return "", fmt.Errorf("%q: omit the default port, browsers do", s)
		}
	}
	if canonical := u.Scheme + "://" + u.Host; canonical != s {
		return "", fmt.Errorf("%q is not in canonical form (%q)", s, canonical)
	}
	return s, nil
}
