# musiclib-tags, the TagLib helper (DESIGN.md §2.1, §8.1).
#
# Built only in Docker, by the Dockerfile stage build-tags (docs/docker.md):
#
#   make check                                   unit tests of the helper's own
#                                                parsers, under ASan/UBSan
#   make MODE=release TAGLIB=<prefix> TAGLIB_BUILD=<rev>
#                                                fully static helper
#   make MODE=asan TAGLIB=<prefix> TAGLIB_BUILD=<rev>
#                                                ASan/UBSan helper, for the
#                                                hostile-input tests only
#
# TAGLIB is the install prefix of a static TagLib 2.x (lib/libtag.a); for
# MODE=asan it must itself be built with the same sanitizers. TAGLIB_BUILD is
# the revision of the TagLib build configuration; it is part of the version
# the helper prints (src/version.h).

MODE ?= release
TAGLIB ?= /opt/taglib
TAGLIB_BUILD ?=
CXX ?= g++
OUT := build/$(MODE)

SRCS := src/main.cpp src/fdio.cpp src/flac.cpp src/inspection.cpp src/json.cpp \
        src/request.cpp src/sha256.cpp src/text.cpp
UNIT_SRCS := tests/unit_tests.cpp src/json.cpp src/sha256.cpp src/text.cpp

WARNINGS := -Wall -Wextra -Wpedantic -Wshadow -Wconversion -Wsign-conversion \
            -Wnon-virtual-dtor -Woverloaded-virtual -Werror
# -ffile-prefix-map keeps build paths out of the binary: the build is
# reproducible byte for byte (NOTES.md N-083).
COMMON := -std=c++20 $(WARNINGS) -ffile-prefix-map=$(CURDIR)=. -isystem $(TAGLIB)/include
SANITIZE := -O1 -g -fno-omit-frame-pointer -fsanitize=address,undefined -fno-sanitize-recover=all

ifeq ($(MODE),release)
  CXXFLAGS := $(COMMON) -O2 -DNDEBUG
  LDFLAGS := -static
else ifeq ($(MODE),asan)
  CXXFLAGS := $(COMMON) $(SANITIZE) -DMUSICLIB_SANITIZERS
  LDFLAGS := $(SANITIZE)
else
  $(error MODE must be release or asan)
endif

.PHONY: all check clean
all: $(OUT)/musiclib-tags

$(OUT)/musiclib-tags: $(SRCS) src/*.h Makefile
	@test -n "$(TAGLIB_BUILD)" || { echo "TAGLIB_BUILD is required" >&2; exit 1; }
	@mkdir -p $(OUT)
	$(CXX) $(CXXFLAGS) -DMUSICLIB_TAGLIB_BUILD='"$(TAGLIB_BUILD)"' -o $@ $(SRCS) src/sanitizers.cpp \
	  $(LDFLAGS) $(TAGLIB)/lib/libtag.a

check: build/unit/unit_tests
	build/unit/unit_tests

build/unit/unit_tests: $(UNIT_SRCS) src/*.h Makefile
	@mkdir -p build/unit
	$(CXX) -std=c++20 $(WARNINGS) $(SANITIZE) -o $@ $(UNIT_SRCS) src/sanitizers.cpp -DMUSICLIB_SANITIZERS

clean:
	rm -rf build
