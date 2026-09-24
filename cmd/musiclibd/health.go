package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	apihttp "musiclib/internal/http"
)

const (
	// readyPingTimeout bounds the database check of /health/ready.
	readyPingTimeout = 2 * time.Second
	// healthcheckTimeout bounds the whole healthcheck request; Compose's
	// own timeout is longer.
	healthcheckTimeout = 3 * time.Second
)

// routes are the health endpoints (§11.1) and the API (§10). Anything else
// is 404. Every response has the security headers of §10.4. The health
// endpoints are outside the API's Host and Origin checks: they carry no
// catalog data and answer probes that address the container by IP, such
// as the healthcheck subcommand (NOTES.md N-145).
func (d *daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", d.handleLive)
	mux.HandleFunc("GET /health/ready", d.handleReady)
	mux.Handle("/api", d.api)
	mux.Handle("/api/", d.api)
	return apihttp.SecurityHeaders(mux)
}

// handleLive says that the process answers (§11.1).
func (d *daemon) handleLive(w http.ResponseWriter, _ *http.Request) {
	d.writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}

// handleReady is positive only when the boot is complete and PostgreSQL
// answers now (§11.1); otherwise 503 (§10.1): not_ready, db_unavailable,
// or publish_illegal_state with the journal's album and build while
// publishing is suspended.
func (d *daemon) handleReady(w http.ResponseWriter, r *http.Request) {
	// §9.4: a journal in an illegal state suspends publishing, and the
	// error is exposed here until the operator acts (N-135).
	if s := d.suspended.Load(); s != nil {
		d.writeJSON(w, http.StatusServiceUnavailable, s)
		return
	}
	pool := d.ready.Load()
	if pool == nil {
		d.writeJSON(w, http.StatusServiceUnavailable,
			errorBody{Code: "not_ready", Message: "boot, recovery or shutdown in progress"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readyPingTimeout)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		d.log.Warn("readiness: database not reachable", "error", err.Error())
		d.writeJSON(w, http.StatusServiceUnavailable,
			errorBody{Code: "db_unavailable", Message: "the database is not reachable"})
		return
	}
	d.writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// errorBody follows the {code, message, details} convention of §10.1.
// Details never carry an absolute path.
type errorBody struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

func (d *daemon) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The client went away: nothing left to tell it.
		d.log.Debug("writing a health response", "error", err.Error())
	}
}

// healthcheck is the `musiclibd healthcheck` subcommand (N-027): the
// runtime image has no curl. It queries /health/ready on HTTP_ADDR and
// returns 0 for 200, 1 for anything else. It reads only HTTP_ADDR, takes no
// lock and starts nothing (§11.3).
func healthcheck(log *slog.Logger, getenv func(string) string) int {
	addr := getenv(envHTTPAddr)
	if addr == "" {
		addr = defaultHTTPAddr
	}
	if err := checkHTTPAddr(addr); err != nil {
		log.Error("healthcheck: "+envHTTPAddr+": "+err.Error(), "code", codeConfig)
		return exitFailure
	}
	url := "http://" + healthTarget(addr) + "/health/ready"
	client := &http.Client{
		Timeout: healthcheckTimeout,
		// No proxy from the environment, no keep-alive, no redirects: the
		// answer must come from this server's own endpoint.
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(url)
	if err != nil {
		log.Error("healthcheck: "+err.Error(), "code", "unhealthy")
		return exitFailure
	}
	_, rerr := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if err := errors.Join(rerr, resp.Body.Close()); err != nil {
		log.Error("healthcheck: reading the response: "+err.Error(), "code", "unhealthy")
		return exitFailure
	}
	if resp.StatusCode != http.StatusOK {
		log.Error("healthcheck: "+resp.Status, "code", "unhealthy")
		return exitFailure
	}
	return exitOK
}

// healthTarget turns the listen address into an address to connect to: an
// empty or unspecified host (":8080", "0.0.0.0:8080", "[::]:8080") becomes
// the loopback address of the same family.
func healthTarget(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	ip := net.ParseIP(host)
	switch {
	case host == "":
		host = "127.0.0.1"
	case ip != nil && ip.IsUnspecified() && ip.To4() == nil:
		host = "::1"
	case ip != nil && ip.IsUnspecified():
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
