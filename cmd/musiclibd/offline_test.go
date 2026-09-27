package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"musiclib/internal/maintenance"
	"musiclib/internal/store"
)

// A maintenance error names the failed operation; the store, volume or media
// cause it wraps must not replace that stable code in the logs (N-225).
func TestCodeOfPrefersMaintenanceCode(t *testing.T) {
	cause := &store.Error{Code: store.CodeMigrate, Msg: "cause"}
	err := &maintenance.Error{Code: "restore_migrate", Message: "wrapped", Err: cause}
	if got := codeOf(errors.Join(err, errors.New("close"))); got != "restore_migrate" {
		t.Fatalf("codeOf = %q, want restore_migrate", got)
	}
	if got := codeOf(cause); got != store.CodeMigrate {
		t.Fatalf("codeOf(cause) = %q", got)
	}
}

func TestOperationExit(t *testing.T) {
	for _, tc := range []struct {
		err    error
		want   int
		code   string
		advice bool
	}{
		{&maintenance.Error{Code: "backup_exists", Message: "m", Refusal: true}, exitUsage, "backup_exists", false},
		{&maintenance.Error{Code: "restore_dump_failed", Message: "m", Err: errors.New("x")}, exitFailure, "restore_dump_failed", true},
		{errors.New("untyped"), exitFailure, "internal", true},
	} {
		var logs bytes.Buffer
		if got := operationExit(newLogger(&logs), tc.err, "the advice"); got != tc.want ||
			!strings.Contains(logs.String(), `"code":"`+tc.code+`"`) ||
			strings.Contains(logs.String(), "the advice") != tc.advice {
			t.Fatalf("%v: exit %d, logs %s", tc.err, got, &logs)
		}
	}
}
