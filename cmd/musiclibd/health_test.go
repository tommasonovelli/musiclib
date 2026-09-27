package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHealthTarget(t *testing.T) {
	for addr, want := range map[string]string{
		":8080":            "127.0.0.1:8080",
		"0.0.0.0:8080":     "127.0.0.1:8080",
		"[::]:8080":        "[::1]:8080",
		"127.0.0.1:9000":   "127.0.0.1:9000",
		"192.168.1.5:8080": "192.168.1.5:8080",
		"[::1]:8080":       "[::1]:8080",
		"localhost:8080":   "localhost:8080",
	} {
		if got := healthTarget(addr); got != want {
			t.Errorf("healthTarget(%q) = %q, want %q", addr, got, want)
		}
	}
}

// The healthcheck subcommand exits 0 only for a 200 from /health/ready on
// HTTP_ADDR (N-027); every other outcome is 1, the "unhealthy" of Docker.
func TestHealthcheckExitCodes(t *testing.T) {
	var status atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/ok": // the redirect target: 200 if it were followed
			w.WriteHeader(http.StatusOK)
		case r.URL.Path != "/health/ready":
			http.NotFound(w, r)
		case status.Load() == http.StatusFound:
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			w.WriteHeader(int(status.Load()))
		}
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	closed := closedPort(t)

	for _, tc := range []struct {
		name   string
		addr   string
		status int
		want   int
	}{
		{"ready", "127.0.0.1:" + port, http.StatusOK, exitOK},
		{"unspecified host", "0.0.0.0:" + port, http.StatusOK, exitOK},
		{"empty host", ":" + port, http.StatusOK, exitOK},
		{"not ready", "127.0.0.1:" + port, http.StatusServiceUnavailable, exitFailure},
		{"redirect not followed", "127.0.0.1:" + port, http.StatusFound, exitFailure},
		{"server error", "127.0.0.1:" + port, http.StatusInternalServerError, exitFailure},
		{"nothing listening", "127.0.0.1:" + closed, http.StatusOK, exitFailure},
		{"invalid HTTP_ADDR", "nonsense", http.StatusOK, exitFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status.Store(int64(tc.status))
			var logs syncBuffer
			got := healthcheck(newLogger(&logs), env(map[string]string{envHTTPAddr: tc.addr}))
			if got != tc.want {
				t.Fatalf("exit %d, want %d; logs: %s", got, tc.want, &logs)
			}
			if got != exitOK && !strings.Contains(logs.String(), `"level":"ERROR"`) {
				t.Fatalf("a failed healthcheck logged nothing: %s", &logs)
			}
		})
	}
}

// closedPort returns a loopback port with nothing listening on it.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{{"serve"}, {"healthcheck", "extra"}, {"doctor", "--unknown"}, {"doctor", "--deep", "extra"}, {"rebuild"}, {"rebuild", "--store-id", "not-a-uuid"}, {"--help"}} {
		var logs syncBuffer
		if got := musiclibd(args, env(nil), &logs); got != exitUsage {
			t.Fatalf("musiclibd %q = %d, want %d", args, got, exitUsage)
		}
		if !strings.Contains(logs.String(), `"code":"usage"`) {
			t.Fatalf("musiclibd %q logged %s", args, &logs)
		}
	}
}

// An invalid configuration stops the server before it listens or touches
// the volume, with exit code 2 and without the password in the logs.
func TestServeRejectsInvalidConfig(t *testing.T) {
	p := testPaths(t)
	e := validEnv()
	e[envDatabaseURL] = "postgres://u:secret-pw@host:port/db"
	var logs syncBuffer
	if got := serve(newLogger(&logs), env(e), p); got != exitUsage {
		t.Fatalf("exit %d, want %d", got, exitUsage)
	}
	out := logs.String()
	if !strings.Contains(out, `"code":"config_invalid"`) || strings.Contains(out, "secret-pw") {
		t.Fatalf("logs: %s", out)
	}
	if exists(t, p.data+"/.lock") {
		t.Fatal("the volume was touched with an invalid configuration")
	}
}
