// Defaults of the sanitizer runtimes, compiled into the sanitized builds only
// (MODE=asan and the unit tests). The Go Runner starts tools with an empty
// environment, so these are the options that apply: any finding stops the
// process with exit status 86, never the helper's own failure status 3, and
// the hostile-input tests treat it as a failure.
#ifdef MUSICLIB_SANITIZERS

extern "C" {

const char *__asan_default_options() { return "exitcode=86:abort_on_error=0:detect_leaks=1"; }

const char *__ubsan_default_options() { return "halt_on_error=1:print_stacktrace=1:exitcode=86"; }

const char *__lsan_default_options() { return "exitcode=86"; }
}

#endif
