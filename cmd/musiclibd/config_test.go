package main

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// validEnv is a complete, valid environment; cases override single keys.
func validEnv() map[string]string {
	return map[string]string{
		envDatabaseURL:  "postgres://musiclib:secret-pw@postgres:5432/musiclib?sslmode=disable",
		envPublicOrigin: "http://127.0.0.1:8080",
		envHTTPAddr:     ":8080",
		envWorkers:      "2",
	}
}

func TestLoadConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  map[string]string
		cpus int
		want Config // compared when err == ""
		err  string // substring of the error; "" = valid
	}{
		{name: "valid", cpus: 8, want: Config{HTTPAddr: ":8080", PublicOrigin: "http://127.0.0.1:8080", Workers: 2}},
		{name: "keyword DSN", set: map[string]string{envDatabaseURL: "host=postgres user=musiclib dbname=musiclib"}, cpus: 8,
			want: Config{HTTPAddr: ":8080", PublicOrigin: "http://127.0.0.1:8080", Workers: 2}},
		{name: "defaults", set: map[string]string{envHTTPAddr: "", envWorkers: ""}, cpus: 8,
			want: Config{HTTPAddr: ":8080", PublicOrigin: "http://127.0.0.1:8080", Workers: 4}},
		{name: "workers default, 2 cpus", set: map[string]string{envWorkers: ""}, cpus: 2,
			want: Config{HTTPAddr: ":8080", PublicOrigin: "http://127.0.0.1:8080", Workers: 2}},
		{name: "workers default, 1 cpu", set: map[string]string{envWorkers: ""}, cpus: 1,
			want: Config{HTTPAddr: ":8080", PublicOrigin: "http://127.0.0.1:8080", Workers: 1}},
		{name: "workers 1", set: map[string]string{envWorkers: "1"}, cpus: 8,
			want: Config{HTTPAddr: ":8080", PublicOrigin: "http://127.0.0.1:8080", Workers: 1}},
		{name: "workers 16", set: map[string]string{envWorkers: "16"}, cpus: 8,
			want: Config{HTTPAddr: ":8080", PublicOrigin: "http://127.0.0.1:8080", Workers: 16}},
		{name: "https origin with port", set: map[string]string{envPublicOrigin: "https://music.lan:8443"}, cpus: 8,
			want: Config{HTTPAddr: ":8080", PublicOrigin: "https://music.lan:8443", Workers: 2}},
		{name: "ipv6 origin", set: map[string]string{envPublicOrigin: "http://[::1]:8080"}, cpus: 8,
			want: Config{HTTPAddr: ":8080", PublicOrigin: "http://[::1]:8080", Workers: 2}},
		{name: "addr with host", set: map[string]string{envHTTPAddr: "127.0.0.1:9000"}, cpus: 8,
			want: Config{HTTPAddr: "127.0.0.1:9000", PublicOrigin: "http://127.0.0.1:8080", Workers: 2}},

		{name: "database missing", set: map[string]string{envDatabaseURL: ""}, err: "DATABASE_URL is required"},
		{name: "database malformed", set: map[string]string{envDatabaseURL: "postgres://u:secret-pw@host:port/db"},
			err: "DATABASE_URL is not a valid"},
		{name: "origin missing", set: map[string]string{envPublicOrigin: ""}, err: "PUBLIC_ORIGIN: required"},
		{name: "origin trailing slash", set: map[string]string{envPublicOrigin: "http://127.0.0.1:8080/"}, err: "no path"},
		{name: "origin path", set: map[string]string{envPublicOrigin: "http://host/app"}, err: "no path"},
		{name: "origin query", set: map[string]string{envPublicOrigin: "http://host?x=1"}, err: "no path"},
		{name: "origin fragment", set: map[string]string{envPublicOrigin: "http://host#x"}, err: "no path"},
		{name: "origin scheme", set: map[string]string{envPublicOrigin: "ftp://host"}, err: "http or https"},
		{name: "origin no scheme", set: map[string]string{envPublicOrigin: "127.0.0.1:8080"}, err: "PUBLIC_ORIGIN"},
		{name: "origin uppercase scheme", set: map[string]string{envPublicOrigin: "HTTP://host"}, err: "canonical"},
		{name: "origin uppercase host", set: map[string]string{envPublicOrigin: "http://Host"}, err: "lowercase ASCII"},
		{name: "origin unicode host", set: map[string]string{envPublicOrigin: "http://müsic.lan"}, err: "lowercase ASCII"},
		{name: "origin userinfo", set: map[string]string{envPublicOrigin: "http://u:p@host"}, err: "user information"},
		{name: "origin default port", set: map[string]string{envPublicOrigin: "http://host:80"}, err: "default port"},
		{name: "origin default https port", set: map[string]string{envPublicOrigin: "https://host:443"}, err: "default port"},
		{name: "origin bad port", set: map[string]string{envPublicOrigin: "http://host:99999"}, err: "port"},
		{name: "origin empty port", set: map[string]string{envPublicOrigin: "http://host:"}, err: "port"},
		{name: "origin leading zero port", set: map[string]string{envPublicOrigin: "http://host:08080"}, err: "port"},
		{name: "origin no host", set: map[string]string{envPublicOrigin: "http://"}, err: "host"},
		{name: "addr no port", set: map[string]string{envHTTPAddr: "127.0.0.1"}, err: "HTTP_ADDR"},
		{name: "addr port zero", set: map[string]string{envHTTPAddr: ":0"}, err: "HTTP_ADDR"},
		{name: "addr port name", set: map[string]string{envHTTPAddr: ":http"}, err: "HTTP_ADDR"},
		{name: "addr port too big", set: map[string]string{envHTTPAddr: ":65536"}, err: "HTTP_ADDR"},
		{name: "workers 0", set: map[string]string{envWorkers: "0"}, err: "outside 1..16"},
		{name: "workers 17", set: map[string]string{envWorkers: "17"}, err: "outside 1..16"},
		{name: "workers negative", set: map[string]string{envWorkers: "-1"}, err: "outside 1..16"},
		{name: "workers plus", set: map[string]string{envWorkers: "+2"}, err: "plain decimal"},
		{name: "workers leading zero", set: map[string]string{envWorkers: "02"}, err: "plain decimal"},
		{name: "workers space", set: map[string]string{envWorkers: " 2"}, err: "plain decimal"},
		{name: "workers word", set: map[string]string{envWorkers: "four"}, err: "plain decimal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnv()
			for k, v := range tc.set {
				e[k] = v
			}
			cfg, err := loadConfig(env(e), tc.cpus)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error %v, want one containing %q", err, tc.err)
				}
				if codeOf(err) != codeConfig {
					t.Fatalf("code %q", codeOf(err))
				}
				if strings.Contains(err.Error(), "secret-pw") {
					t.Fatalf("the error leaks the password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.want.DatabaseURL = e[envDatabaseURL]
			if cfg != tc.want {
				t.Fatalf("config %+v, want %+v", cfg, tc.want)
			}
		})
	}
}

// Every problem is reported at once.
func TestLoadConfigReportsAllProblems(t *testing.T) {
	_, err := loadConfig(env(map[string]string{envHTTPAddr: "x", envWorkers: "99"}), 4)
	for _, want := range []string{envDatabaseURL, envPublicOrigin, envHTTPAddr, envWorkers} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v does not mention %s", err, want)
		}
	}
}

func TestDefaultWorkers(t *testing.T) {
	for cpus, want := range map[int]int{0: 1, 1: 1, 2: 2, 3: 3, 4: 4, 5: 4, 64: 4} {
		if got := defaultWorkers(cpus); got != want {
			t.Fatalf("defaultWorkers(%d) = %d, want %d", cpus, got, want)
		}
	}
}

// The sign-in password: required, at least 12 characters, at most 1024
// bytes, UTF-8, no control character. Every refusal is password_invalid,
// and no message contains the value.
func TestLoadPassword(t *testing.T) {
	for _, tc := range []struct {
		name, value, err string
	}{
		{"missing", "", "missing or empty"},
		{"11 characters", "abcdefghijk", "shorter than 12 characters"},
		{"11 multi-byte characters", strings.Repeat("é", 11), "shorter than 12 characters"},
		{"a line break", "correct-horse-battery\n", "control character"},
		{"a carriage return", "correct-horse-battery\r", "control character"},
		{"a tab", "correct\thorse-battery", "control character"},
		{"not UTF-8", "correct-horse-\xff-battery", "not valid UTF-8"},
		{"1025 bytes", strings.Repeat("a", 1025), "longer than 1024 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadPassword(env(map[string]string{envPassword: tc.value}))
			if err == nil || got != "" || codeOf(err) != codePassword || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("loadPassword = %q, %v; want %s containing %q", got, err, codePassword, tc.err)
			}
			if tc.value != "" && strings.Contains(err.Error(), tc.value) {
				t.Fatalf("the error contains the value: %v", err)
			}
			if !strings.Contains(err.Error(), envPassword) || !strings.Contains(err.Error(), ".env") {
				t.Fatalf("the error does not say what to set and where: %v", err)
			}
		})
	}
	for _, ok := range []string{"abcdefghijkl", strings.Repeat("é", 12), strings.Repeat("a", 1024), "Zm9vYmFyYmF6cXV1eGZvb2Jhcg+/aa"} {
		if got, err := loadPassword(env(map[string]string{envPassword: ok})); err != nil || got != ok {
			t.Fatalf("loadPassword(%q) = %q, %v", ok, got, err)
		}
	}
}

// The configuration is logged without DATABASE_URL and without the
// sign-in password.
func TestConfigLogValueOmitsDatabaseURL(t *testing.T) {
	cfg, err := loadConfig(env(validEnv()), 4)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = "secret-sign-in-password"
	var logs syncBuffer
	newLogger(&logs).Info("starting", "config", cfg)
	out := logs.String()
	if strings.Contains(out, "secret-pw") || strings.Contains(out, "postgres://") {
		t.Fatalf("the log contains the database URL: %s", out)
	}
	if strings.Contains(out, "secret-sign-in-password") {
		t.Fatalf("the log contains the sign-in password: %s", out)
	}
	for _, want := range []string{`"workers":2`, `"http_addr":":8080"`, `"public_origin":"http://127.0.0.1:8080"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("the log lacks %s: %s", want, out)
		}
	}
}

func TestCheckNotRoot(t *testing.T) {
	if err := checkNotRoot(0); codeOf(err) != codeRoot {
		t.Fatalf("uid 0: %v", err)
	}
	if err := checkNotRoot(1000); err != nil {
		t.Fatalf("uid 1000: %v", err)
	}
}
