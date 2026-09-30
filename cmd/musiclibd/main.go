// Command musiclibd is the musiclib server (DESIGN.md §2.3, §11.1).
//
//	musiclibd              run the server
//	musiclibd healthcheck  query /health/ready on HTTP_ADDR; exit 0 or 1
//	musiclibd version      print the version and render_version
//
// Configuration comes only from the environment (§11.1): DATABASE_URL,
// PUBLIC_ORIGIN, HTTP_ADDR (default ":8080") and WORKERS (default
// max(1, min(4, CPUs)), 1..16, §6.1). The server alone also needs
// MUSICLIB_PASSWORD, the sign-in password: the offline commands, version and
// healthcheck run without it. The data volume is always /data and the
// import source always /import. Logs are JSON lines on stderr.
//
// Offline doctor, rebuild, backup and restore take the volume lock and never
// start the server (DESIGN.md §11.3–§11.4).
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"

	"github.com/google/uuid"

	"golang.org/x/sys/unix"

	"musiclib/internal/blobstore"
	"musiclib/internal/buildinfo"
	"musiclib/internal/fsops"
	"musiclib/internal/maintenance"
	"musiclib/internal/media"
	"musiclib/internal/publish"
	"musiclib/internal/render"
	"musiclib/internal/store"
	"musiclib/internal/volume"
)

// Fixed internal paths (§11.1). Only the tests use others, through paths.
const (
	dataPath   = "/data"
	importPath = "/import"
)

// Exit codes. healthcheck only ever uses 0 and 1, the two values Docker
// defines for a healthcheck.
const (
	exitOK      = 0
	exitFailure = 1 // fatal boot or runtime error
	exitUsage   = 2 // bad command line, invalid configuration, running as root
)

// paths are the host paths the server knows: the two volumes (§3.1, §11.1)
// and the pinned native tools (§2.1). They are fixed in production; only the
// tests use others.
type paths struct {
	data    string
	imports string
	ffmpeg  string
	ffprobe string
	tags    string
}

// defaultPaths are the fixed paths of the images.
var defaultPaths = paths{data: dataPath, imports: importPath, ffmpeg: media.FFmpegPath, ffprobe: media.FFprobePath, tags: media.TagsPath}

func main() {
	os.Exit(musiclibd(os.Args[1:], os.Getenv, os.Stderr))
}

func musiclibd(args []string, getenv func(string) string, stderr io.Writer) int {
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	switch {
	case len(args) == 0:
		return serve(log, getenv, defaultPaths)
	case len(args) == 1 && args[0] == "healthcheck":
		return healthcheck(log, getenv)
	case len(args) == 1 && args[0] == "version":
		return printVersion(os.Stdout, log)
	case len(args) == 1 && args[0] == "doctor":
		return runDoctor(getenv, defaultPaths, false, os.Stdout, log)
	case len(args) == 2 && args[0] == "doctor" && args[1] == "--deep":
		return runDoctor(getenv, defaultPaths, true, os.Stdout, log)
	case len(args) == 3 && args[0] == "backup" && args[1] == "--to":
		if err := checkBackupDestination(args[2]); err != nil {
			logFatal(log, err)
			return exitUsage
		}
		return runBackup(getenv, defaultPaths, args[2], os.Stdout, log)
	case len(args) == 3 && args[0] == "restore" && args[1] == "--from":
		return runRestore(getenv, defaultPaths, args[2], os.Stdout, log)
	case len(args) == 3 && args[0] == "rebuild" && args[1] == "--store-id":
		id, err := uuid.Parse(args[2])
		if err == nil && id != uuid.Nil && id.String() == args[2] {
			return runRebuild(getenv, defaultPaths, id, os.Stdout, log)
		}
		log.Error("rebuild requires a canonical non-nil UUID for --store-id", "code", "usage")
		return exitUsage
	default:
		log.Error("usage: musiclibd [healthcheck|version|doctor [--deep]|rebuild --store-id UUID|backup --to /backup/NAME|restore --from /backup/NAME]", "code", "usage", "args", args)
		return exitUsage
	}
}

// serve runs the server until SIGTERM or SIGINT.
func serve(log *slog.Logger, getenv func(string) string, p paths) int {
	// §11.1 and N-020: whatever the runtime or entrypoint set.
	unix.Umask(0o022)
	if err := checkNotRoot(os.Geteuid()); err != nil {
		logFatal(log, err)
		return exitUsage
	}
	// GOMAXPROCS is the number of CPUs available to the process: since Go
	// 1.25 it honors the cgroup CPU limit as well as the affinity (N-066).
	cfg, err := loadConfig(getenv, runtime.GOMAXPROCS(0))
	password, perr := loadPassword(getenv)
	if err != nil || perr != nil {
		// Both problems at once, each with its own code.
		for _, e := range []error{err, perr} {
			if e != nil {
				logFatal(log, e)
			}
		}
		return exitUsage
	}
	cfg.Password = password
	log.Info("starting", "version", buildinfo.Version, "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGTERM, unix.SIGINT)
	defer stop()
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		logFatal(log, &bootError{code: codeHTTP, msg: "cannot listen on " + cfg.HTTPAddr, err: err})
		return exitFailure
	}
	if err := run(ctx, cfg, p, ln, log); err != nil {
		logFatal(log, err)
		return exitFailure
	}
	log.Info("stopped")
	return exitOK
}

// checkNotRoot refuses uid 0 (§11.1: "niente esecuzione come root"): root
// bypasses the permission checks the boot relies on and would create files
// the configured user cannot touch later.
func checkNotRoot(euid int) error {
	if euid == 0 {
		return &bootError{code: codeRoot, msg: "musiclibd must not run as root: " +
			"set an unprivileged user (Compose `user: UID:GID`, DESIGN.md §11.1)"}
	}
	return nil
}

// logFatal logs the error that stops the process, with its stable code.
func logFatal(log *slog.Logger, err error) {
	log.Error(err.Error(), "code", codeOf(err))
}

// bootError is the server's own typed error, for the failures that belong
// to no package: configuration, the listener, /import.
type bootError struct {
	code string
	msg  string
	err  error
}

const (
	codeRoot   = "run_as_root"
	codeHTTP   = "http_listen"
	codeImport = "import_unavailable"
	// codePassword: MUSICLIB_PASSWORD is missing or unusable.
	codePassword = "password_invalid"
	// codeImportIsData: /import is /data or one of its directories (§7.1).
	codeImportIsData = "import_is_data"
	// codeWorkers: the worker pool stopped without an error of its own.
	codeWorkers = "workers_stopped"
)

func (e *bootError) Error() string {
	if e.err == nil {
		return e.code + ": " + e.msg
	}
	return e.code + ": " + e.msg + ": " + e.err.Error()
}

func (e *bootError) Unwrap() error { return e.err }

// codeOf returns the stable code of a fatal error: that of the outermost
// typed error of this program or of the package that failed.
func codeOf(err error) string {
	var (
		be *bootError
		ve *volume.Error
		se *store.Error
		bl *blobstore.Error
		me *media.Error
		mt *maintenance.Error
		re *render.Error
	)
	switch {
	case errors.As(err, &be):
		return be.code
	// A maintenance error is the outer operation's code; it may wrap a
	// volume, store or media cause that must not replace it.
	case errors.As(err, &mt):
		return mt.Code
	case errors.As(err, &ve):
		return ve.Code
	case errors.As(err, &se):
		return se.Code
	case publish.Code(err) != "":
		return publish.Code(err)
	case errors.As(err, &bl):
		return bl.Code
	case errors.As(err, &me):
		return me.Code
	case errors.As(err, &re):
		return re.Code
	case fsops.Code(err) != "":
		return fsops.Code(err)
	default:
		return "internal"
	}
}
