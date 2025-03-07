# Make targets:
#
#  all    : builds all binaries in development mode, without web assets (default)
#  full   : builds all binaries for PRODUCTION use
#  release: prepares a release tarball
#  clean  : removes all buld artifacts
#  test   : runs tests

# To update the Teleport version, update VERSION variable:
# Naming convention:
#   Stable releases:   "1.0.0"
#   Pre-releases:      "1.0.0-alpha.1", "1.0.0-beta.2", "1.0.0-rc.3"
#   Master/dev branch: "1.0.0-dev"
VERSION=11.0.0-dev

DOCKER_IMAGE_QUAY ?= quay.io/gravitational/teleport
DOCKER_IMAGE_ECR ?= public.ecr.aws/gravitational/teleport
DOCKER_IMAGE_STAGING ?= 146628656107.dkr.ecr.us-west-2.amazonaws.com/gravitational/teleport
DOCKER_IMAGE_OPERATOR_STAGING ?= 146628656107.dkr.ecr.us-west-2.amazonaws.com/gravitational/teleport-operator


GOPATH ?= $(shell go env GOPATH)

# These are standard autotools variables, don't change them please
ifneq ("$(wildcard /bin/bash)","")
SHELL := /bin/bash -o pipefail
endif
BUILDDIR ?= build
ASSETS_BUILDDIR ?= lib/web/build
BINDIR ?= /usr/local/bin
DATADIR ?= /usr/local/share/teleport
ADDFLAGS ?=
PWD ?= `pwd`
TELEPORT_DEBUG ?= false
GITTAG=v$(VERSION)
BUILDFLAGS ?= $(ADDFLAGS) -ldflags '-w -s'
CGOFLAG ?= CGO_ENABLED=1
CGOFLAG_TSH ?= CGO_ENABLED=1
# Windows requires extra parameters to cross-compile with CGO.
ifeq ("$(OS)","windows")
ARCH ?= amd64
ifneq ("$(ARCH)","amd64")
$(error "Building for windows requires ARCH=amd64")
endif
BUILDFLAGS = $(ADDFLAGS) -ldflags '-w -s' -buildmode=exe
CGOFLAG = CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++
CGOFLAG_TSH = $(CGOFLAG)
endif

ifeq ("$(OS)","linux")
# Link static version of libgcc to reduce system dependencies.
CGOFLAG ?= CGO_ENABLED=1 CGO_LDFLAGS="-Wl,--as-needed"
CGOFLAG_TSH ?= CGO_ENABLED=1 CGO_LDFLAGS="-Wl,--as-needed"
# ARM builds need to specify the correct C compiler
ifeq ("$(ARCH)","arm")
CGOFLAG = CGO_ENABLED=1 CC=arm-linux-gnueabihf-gcc
CGOFLAG_TSH = $(CGOFLAG)
endif
# ARM64 builds need to specify the correct C compiler
ifeq ("$(ARCH)","arm64")
CGOFLAG = CGO_ENABLED=1 CC=aarch64-linux-gnu-gcc
CGOFLAG_TSH = $(CGOFLAG)
endif
endif

OS ?= $(shell go env GOOS)
ARCH ?= $(shell go env GOARCH)
FIPS ?=
RELEASE = teleport-$(GITTAG)-$(OS)-$(ARCH)-bin

# FIPS support must be requested at build time.
FIPS_MESSAGE := "without FIPS support"
ifneq ("$(FIPS)","")
FIPS_TAG := fips
FIPS_MESSAGE := "with FIPS support"
RELEASE = teleport-$(GITTAG)-$(OS)-$(ARCH)-fips-bin
endif

# PAM support will only be built into Teleport if headers exist at build time.
PAM_MESSAGE := "without PAM support"
ifneq ("$(wildcard /usr/include/security/pam_appl.h)","")
PAM_TAG := pam
PAM_MESSAGE := "with PAM support"
else
# PAM headers for Darwin live under /usr/local/include/security instead, as SIP
# prevents us from modifying/creating /usr/include/security on newer versions of MacOS
ifneq ("$(wildcard /usr/local/include/security/pam_appl.h)","")
PAM_TAG := pam
PAM_MESSAGE := "with PAM support"
endif
endif

# BPF support will only be built into Teleport if headers exist at build time.
BPF_MESSAGE := "without BPF support"

# We don't compile BPF for anything except regular non-FIPS linux/amd64 for now, as other builds
# have compilation issues that require fixing.
with_bpf := no
ifeq ("$(OS)","linux")
ifeq ("$(ARCH)","amd64")
ifneq ("$(wildcard /usr/include/bpf/libbpf.h)","")
with_bpf := yes
BPF_TAG := bpf
BPF_MESSAGE := "with BPF support"
CLANG ?= $(shell which clang || which clang-10)
CLANG_FORMAT ?= $(shell which clang-format || which clang-format-10)
LLVM_STRIP ?= $(shell which llvm-strip || which llvm-strip-10)
KERNEL_ARCH := $(shell uname -m | sed 's/x86_64/x86/')
INCLUDES :=
ER_BPF_BUILDDIR := lib/bpf/bytecode
RS_BPF_BUILDDIR := lib/restrictedsession/bytecode

# Get Clang's default includes on this system. We'll explicitly add these dirs
# to the includes list when compiling with `-target bpf` because otherwise some
# architecture-specific dirs will be "missing" on some architectures/distros -
# headers such as asm/types.h, asm/byteorder.h, asm/socket.h, asm/sockios.h,
# sys/cdefs.h etc. might be missing.
#
# Use '-idirafter': Don't interfere with include mechanics except where the
# build would have failed anyways.
CLANG_BPF_SYS_INCLUDES = $(shell $(CLANG) -v -E - </dev/null 2>&1 \
	| sed -n '/<...> search starts here:/,/End of search list./{ s| \(/.*\)|-idirafter \1|p }')

CGOFLAG = CGO_ENABLED=1 CGO_LDFLAGS="-Wl,-Bstatic -lbpf -lelf -lz -Wl,-Bdynamic -Wl,--as-needed"
CGOFLAG_TSH = CGO_ENABLED=1
endif
endif
endif

# Check if rust and cargo are installed before compiling
CHECK_CARGO := $(shell cargo --version 2>/dev/null)
CHECK_RUST := $(shell rustc --version 2>/dev/null)

with_rdpclient := no
RDPCLIENT_MESSAGE := "without Windows RDP client"

CARGO_TARGET_darwin_amd64 := x86_64-apple-darwin
CARGO_TARGET_darwin_arm64 := aarch64-apple-darwin
CARGO_TARGET_linux_arm := arm-unknown-linux-gnueabihf
CARGO_TARGET_linux_arm64 := aarch64-unknown-linux-gnu
CARGO_TARGET_linux_386 := i686-unknown-linux-gnu
CARGO_TARGET_linux_amd64 := x86_64-unknown-linux-gnu

CARGO_TARGET := --target=${CARGO_TARGET_${OS}_${ARCH}}

ifneq ($(CHECK_RUST),)
ifneq ($(CHECK_CARGO),)

ifneq ("$(ARCH)","arm")
# Do not build RDP client on ARM.
with_rdpclient := yes
RDPCLIENT_MESSAGE := "with Windows RDP client"
RDPCLIENT_TAG := desktop_access_rdp
endif
endif
endif

# Enable libfido2 for testing?
# Eargerly enable if we detect the package, we want to test as much as possible.
ifeq ("$(shell pkg-config libfido2 2>/dev/null; echo $$?)", "0")
LIBFIDO2_TEST_TAG := libfido2
endif

# Build tsh against libfido2?
# FIDO2=yes and FIDO2=static enable static libfido2 builds.
# FIDO2=dynamic enables dynamic libfido2 builds.
LIBFIDO2_MESSAGE := without libfido2
ifneq (, $(filter $(FIDO2), yes static))
LIBFIDO2_MESSAGE := with libfido2
LIBFIDO2_BUILD_TAG := libfido2 libfido2static
else ifeq ("$(FIDO2)", "dynamic")
LIBFIDO2_MESSAGE := with libfido2
LIBFIDO2_BUILD_TAG := libfido2
endif

# Enable Touch ID builds?
# Only build if TOUCHID=yes to avoid issues when cross-compiling to 'darwin'
# from other systems.
TOUCHID_MESSAGE := without Touch ID
ifeq ("$(TOUCHID)", "yes")
TOUCHID_MESSAGE := with Touch ID
TOUCHID_TAG := touchid
endif

# Reproducible builds are only available on select targets, and only when OS=linux.
REPRODUCIBLE ?=
ifneq ("$(OS)","linux")
REPRODUCIBLE = no
endif

# On Windows only build tsh. On all other platforms build teleport, tctl,
# and tsh.
BINARIES=$(BUILDDIR)/teleport $(BUILDDIR)/tctl $(BUILDDIR)/tsh $(BUILDDIR)/tbot
RELEASE_MESSAGE := "Building with GOOS=$(OS) GOARCH=$(ARCH) REPRODUCIBLE=$(REPRODUCIBLE) and $(PAM_MESSAGE) and $(FIPS_MESSAGE) and $(BPF_MESSAGE) and $(RDPCLIENT_MESSAGE) and $(LIBFIDO2_MESSAGE) and $(TOUCHID_MESSAGE)."
ifeq ("$(OS)","windows")
BINARIES=$(BUILDDIR)/tsh
endif

# On platforms that support reproducible builds, ensure the archive is created in a reproducible manner.
TAR_FLAGS ?=
ifeq ("$(REPRODUCIBLE)","yes")
TAR_FLAGS = --sort=name --owner=root:0 --group=root:0 --mtime='UTC 2015-03-02' --format=gnu
endif

VERSRC = version.go gitref.go api/version.go

KUBECONFIG ?=
TEST_KUBE ?=
export

TEST_LOG_DIR = ${abspath ./test-logs}

CLANG_FORMAT_STYLE = '{ColumnLimit: 100, IndentWidth: 4, Language: Proto}'

#
# 'make all' builds all 3 executables and places them in the current directory.
#
# IMPORTANT: the binaries will not contain the web UI assets and `teleport`
#            won't start without setting the environment variable DEBUG=1
#            This is the default build target for convenience of working on
#            a web UI.
.PHONY: all
all: version
	@echo "---> Building OSS binaries."
	$(MAKE) $(BINARIES)

# By making these 3 targets below (tsh, tctl and teleport) PHONY we are solving
# several problems:
# * Build will rely on go build internal caching https://golang.org/doc/go1.10 at all times
# * Manual change detection was broken on a large dependency tree
# If you are considering changing this behavior, please consult with dev team first
.PHONY: $(BUILDDIR)/tctl
$(BUILDDIR)/tctl:
	GOOS=$(OS) GOARCH=$(ARCH) $(CGOFLAG) go build -tags "$(PAM_TAG) $(FIPS_TAG)" -o $(BUILDDIR)/tctl $(BUILDFLAGS) ./tool/tctl

.PHONY: $(BUILDDIR)/teleport
$(BUILDDIR)/teleport: ensure-webassets bpf-bytecode rdpclient
	GOOS=$(OS) GOARCH=$(ARCH) $(CGOFLAG) go build -tags "$(PAM_TAG) $(FIPS_TAG) $(BPF_TAG) $(WEBASSETS_TAG) $(RDPCLIENT_TAG)" -o $(BUILDDIR)/teleport $(BUILDFLAGS) ./tool/teleport

.PHONY: $(BUILDDIR)/tsh
$(BUILDDIR)/tsh:
	GOOS=$(OS) GOARCH=$(ARCH) $(CGOFLAG_TSH) go build -tags "$(FIPS_TAG) $(LIBFIDO2_BUILD_TAG) $(TOUCHID_TAG)" -o $(BUILDDIR)/tsh $(BUILDFLAGS) ./tool/tsh

.PHONY: $(BUILDDIR)/tbot
$(BUILDDIR)/tbot:
	GOOS=$(OS) GOARCH=$(ARCH) $(CGOFLAG) go build -tags "$(FIPS_TAG)" -o $(BUILDDIR)/tbot $(BUILDFLAGS) ./tool/tbot

#
# BPF support (IF ENABLED)
# Requires a recent version of clang and libbpf installed.
#
ifeq ("$(with_bpf)","yes")
$(ER_BPF_BUILDDIR):
	mkdir -p $(ER_BPF_BUILDDIR)

$(RS_BPF_BUILDDIR):
	mkdir -p $(RS_BPF_BUILDDIR)

# Build BPF code
$(ER_BPF_BUILDDIR)/%.bpf.o: bpf/enhancedrecording/%.bpf.c $(wildcard bpf/*.h) | $(ER_BPF_BUILDDIR)
	$(CLANG) -g -O2 -target bpf -D__TARGET_ARCH_$(KERNEL_ARCH) $(INCLUDES) $(CLANG_BPF_SYS_INCLUDES) -c $(filter %.c,$^) -o $@
	$(LLVM_STRIP) -g $@ # strip useless DWARF info

# Build BPF code
$(RS_BPF_BUILDDIR)/%.bpf.o: bpf/restrictedsession/%.bpf.c $(wildcard bpf/*.h) | $(RS_BPF_BUILDDIR)
	$(CLANG) -g -O2 -target bpf -D__TARGET_ARCH_$(KERNEL_ARCH) $(INCLUDES) $(CLANG_BPF_SYS_INCLUDES) -c $(filter %.c,$^) -o $@
	$(LLVM_STRIP) -g $@ # strip useless DWARF info

.PHONY: bpf-rs-bytecode
bpf-rs-bytecode: $(RS_BPF_BUILDDIR)/restricted.bpf.o

.PHONY: bpf-er-bytecode
bpf-er-bytecode: $(ER_BPF_BUILDDIR)/command.bpf.o $(ER_BPF_BUILDDIR)/disk.bpf.o $(ER_BPF_BUILDDIR)/network.bpf.o $(ER_BPF_BUILDDIR)/counter_test.bpf.o

.PHONY: bpf-bytecode
bpf-bytecode: bpf-er-bytecode bpf-rs-bytecode

# Generate vmlinux.h based on the installed kernel
.PHONY: update-vmlinux-h
update-vmlinux-h:
	bpftool btf dump file /sys/kernel/btf/vmlinux format c >bpf/vmlinux.h

else
.PHONY: bpf-bytecode
bpf-bytecode:
endif

ifeq ("$(with_rdpclient)", "yes")
.PHONY: rdpclient
rdpclient:
	cargo build -p rdp-client --release $(CARGO_TARGET)
else
.PHONY: rdpclient
rdpclient:
endif

#
# make full - Builds Teleport binaries with the built-in web assets and
# places them into $(BUILDDIR). On Windows, this target is skipped because
# only tsh is built.
#
.PHONY:full
full: $(ASSETS_BUILDDIR)/webassets
ifneq ("$(OS)", "windows")
	$(MAKE) all WEBASSETS_TAG="webassets_embed"
endif

#
# make full-ent - Builds Teleport enterprise binaries
#
.PHONY:full-ent
full-ent:
ifneq ("$(OS)", "windows")
	@if [ -f e/Makefile ]; then \
	rm $(ASSETS_BUILDDIR)/webassets; \
	$(MAKE) -C e full; fi
endif

#
# make clean - Removes all build artifacts.
#
.PHONY: clean
clean:
	@echo "---> Cleaning up OSS build artifacts."
	rm -rf $(BUILDDIR)
# Check if the variable is set to prevent calling remove on the root directory.
ifneq ($(ER_BPF_BUILDDIR),)
	rm -f $(ER_BPF_BUILDDIR)/*.o
endif
ifneq ($(RS_BPF_BUILDDIR),)
	rm -f $(RS_BPF_BUILDDIR)/*.o
endif
	-cargo clean
	-go clean -cache
	rm -f *.gz
	rm -f *.zip
	rm -f gitref.go
	rm -rf build.assets/tooling/bin

#
# make release - Produces a binary release tarball.
#
.PHONY:
export
release:
	@echo "---> $(RELEASE_MESSAGE)"
ifeq ("$(OS)", "windows")
	$(MAKE) --no-print-directory release-windows
else
	$(MAKE) --no-print-directory release-unix
endif

# These are aliases used to make build commands uniform.
.PHONY: release-amd64
release-amd64:
	$(MAKE) release ARCH=amd64

.PHONY: release-386
release-386:
	$(MAKE) release ARCH=386

.PHONY: release-arm
release-arm:
	$(MAKE) release ARCH=arm

.PHONY: release-arm64
release-arm64:
	$(MAKE) release ARCH=arm64

#
# make release-unix - Produces a binary release tarball containing teleport,
# tctl, and tsh.
#
.PHONY:
release-unix: clean full
	@echo "---> Creating OSS release archive."
	mkdir teleport
	cp -rf $(BUILDDIR)/* \
		examples \
		build.assets/install\
		README.md \
		CHANGELOG.md \
		teleport/
	echo $(GITTAG) > teleport/VERSION
	tar $(TAR_FLAGS) -c teleport | gzip -n > $(RELEASE).tar.gz
	rm -rf teleport
	@echo "---> Created $(RELEASE).tar.gz."
	@if [ -f e/Makefile ]; then \
		rm -fr $(ASSETS_BUILDDIR)/webassets; \
		$(MAKE) -C e release; \
	fi

#
# make release-windows-unsigned - Produces a binary release archive containing only tsh.
#
.PHONY: release-windows-unsigned
release-windows-unsigned: clean all
	@echo "---> Creating OSS release archive."
	mkdir teleport
	cp -rf $(BUILDDIR)/* \
		README.md \
		CHANGELOG.md \
		teleport/
	mv teleport/tsh teleport/tsh-unsigned.exe
	echo $(GITTAG) > teleport/VERSION
	zip -9 -y -r -q $(RELEASE)-unsigned.zip teleport/
	rm -rf teleport/
	@echo "---> Created $(RELEASE)-unsigned.zip."

#
# make release-windows - Produces an archive containing a signed release of
# tsh.exe
#
.PHONY: release-windows
release-windows: release-windows-unsigned
	@if [ ! -f "windows-signing-cert.pfx" ]; then \
		echo "windows-signing-cert.pfx is missing or invalid, cannot create signed archive."; \
		exit 1; \
	fi

	rm -rf teleport
	@echo "---> Extracting $(RELEASE)-unsigned.zip"
	unzip $(RELEASE)-unsigned.zip

	@echo "---> Signing Windows binary."
	@osslsigncode sign \
		-pkcs12 "windows-signing-cert.pfx" \
		-n "Teleport" \
		-i https://goteleport.com \
		-t http://timestamp.digicert.com \
		-h sha2 \
		-in teleport/tsh-unsigned.exe \
		-out teleport/tsh.exe; \
	success=$$?; \
	rm -f teleport/tsh-unsigned.exe; \
	if [ "$${success}" -ne 0 ]; then \
		echo "Failed to sign tsh.exe, aborting."; \
		exit 1; \
	fi

	zip -9 -y -r -q $(RELEASE).zip teleport/
	rm -rf teleport/
	@echo "---> Created $(RELEASE).zip."

#
# Remove trailing whitespace in all markdown files under docs/.
#
# Note: this runs in a busybox container to avoid incompatibilities between
# linux and macos CLI tools.
#
.PHONY:docs-fix-whitespace
docs-fix-whitespace:
	docker run --rm -v $(PWD):/teleport busybox \
		find /teleport/docs/ -type f -name '*.md' -exec sed -E -i 's/\s+$$//g' '{}' \;

#
# Test docs for trailing whitespace and broken links
#
.PHONY:docs-test
docs-test: docs-test-whitespace

#
# Check for trailing whitespace in all markdown files under docs/
#
.PHONY:docs-test-whitespace
docs-test-whitespace:
	if find docs/ -type f -name '*.md' | xargs grep -E '\s+$$'; then \
		echo "trailing whitespace found in docs/ (see above)"; \
		echo "run 'make docs-fix-whitespace' to fix it"; \
		exit 1; \
	fi

#
# Builds some tooling for filtering and displaying test progress/output/etc
#
TOOLINGDIR := ${abspath ./build.assets/tooling}
RENDER_TESTS := $(TOOLINGDIR)/bin/render-tests
$(RENDER_TESTS): $(wildcard $(TOOLINGDIR)/cmd/render-tests/*.go)
	cd $(TOOLINGDIR) && go build -o "$@" ./cmd/render-tests
#
# Runs all Go/shell tests, called by CI/CD.
#
.PHONY: test
test: test-helm test-sh test-ci test-api test-go test-rust test-operator

# Runs bot Go tests.
#
.PHONY: test-bot
test-bot:
test-bot: FLAGS ?= -race -shuffle on
test-bot:
	cd .github/workflows/robot && go test $(FLAGS) ./...

$(TEST_LOG_DIR):
	mkdir $(TEST_LOG_DIR)

# Google Cloud Build uses a weird homedir and Helm can't pick up plugins by default there,
# so override the plugin location via environment variable when running in CI.
.PHONY: test-helm
test-helm:
	@if [ -d /builder/home ]; then export HELM_PLUGINS=/root/.local/share/helm/plugins; fi; \
		helm unittest examples/chart/teleport-cluster && \
		helm unittest examples/chart/teleport-kube-agent

.PHONY: test-helm-update-snapshots
test-helm-update-snapshots:
	helm unittest -u examples/chart/teleport-cluster
	helm unittest -u examples/chart/teleport-kube-agent

#
# Runs all Go tests except integration, called by CI/CD.
# Chaos tests have high concurrency, run without race detector and have TestChaos prefix.
#
.PHONY: test-go
test-go: ensure-webassets bpf-bytecode rdpclient $(TEST_LOG_DIR) $(RENDER_TESTS)
test-go: FLAGS ?= -race -shuffle on
test-go: PACKAGES = $(shell go list ./... | grep -v -e integration -e tool/tsh -e operator )
test-go: CHAOS_FOLDERS = $(shell find . -type f -name '*chaos*.go' | xargs dirname | uniq)
test-go: $(VERSRC) $(TEST_LOG_DIR)
	$(CGOFLAG) go test -cover -json -tags "$(PAM_TAG) $(FIPS_TAG) $(BPF_TAG) $(RDPCLIENT_TAG) $(TOUCHID_TAG)" $(PACKAGES) $(FLAGS) $(ADDFLAGS) \
		| tee $(TEST_LOG_DIR)/unit.json \
		| ${RENDER_TESTS}
# rdpclient and libfido2 don't play well together, so we run libfido2 tests
# separately.
# TODO(codingllama): Run libfido2 tests along with others once RDP doesn't
#  embed openssl/libcrypto.
ifneq ("$(LIBFIDO2_TEST_TAG)", "")
	$(CGOFLAG) go test -cover -json -tags "$(LIBFIDO2_TEST_TAG)" ./lib/auth/webauthncli/... $(FLAGS) $(ADDFLAGS) \
		| tee $(TEST_LOG_DIR)/unit.json \
		| ${RENDER_TESTS}
endif
# Make sure untagged touchid code build/tests.
ifneq ("$(TOUCHID_TAG)", "")
	$(CGOFLAG) go test -cover -json ./lib/auth/touchid/... $(FLAGS) $(ADDFLAGS) \
		| tee $(TEST_LOG_DIR)/unit.json \
		| ${RENDER_TESTS}
endif
	$(CGOFLAG_TSH) go test -cover -json -tags "$(PAM_TAG) $(FIPS_TAG) $(LIBFIDO2_TEST_TAG) $(TOUCHID_TAG)" github.com/gravitational/teleport/tool/tsh $(FLAGS) $(ADDFLAGS) \
		| tee $(TEST_LOG_DIR)/unit.json \
		| ${RENDER_TESTS}
	$(CGOFLAG) go test -cover -json -tags "$(PAM_TAG) $(FIPS_TAG) $(BPF_TAG) $(RDPCLIENT_TAG)" -test.run=TestChaos $(CHAOS_FOLDERS) \
		| tee $(TEST_LOG_DIR)/chaos.json \
		| ${RENDER_TESTS}

.PHONY: test-ci
test-ci: $(TEST_LOG_DIR) $(RENDER_TESTS)
	(cd .cloudbuild/scripts && \
		go test -cover -json ./... \
		| tee $(TEST_LOG_DIR)/ci.json \
		| ${RENDER_TESTS})

#
# Runs all Go tests except integration and chaos, called by CI/CD.
#
UNIT_ROOT_REGEX := ^TestRoot
.PHONY: test-go-root
test-go-root: ensure-webassets bpf-bytecode rdpclient $(TEST_LOG_DIR) $(RENDER_TESTS)
test-go-root: FLAGS ?= -race -shuffle on
test-go-root: PACKAGES = $(shell go list $(ADDFLAGS) ./... | grep -v -e integration -e operator)
test-go-root: $(VERSRC)
	$(CGOFLAG) go test -json -run "$(UNIT_ROOT_REGEX)" -tags "$(PAM_TAG) $(FIPS_TAG) $(BPF_TAG) $(RDPCLIENT_TAG)" $(PACKAGES) $(FLAGS) $(ADDFLAGS)
		| tee $(TEST_LOG_DIR)/unit-root.json \
		| ${RENDER_TESTS}

#
# Runs Go tests on the api module. These have to be run separately as the package name is different.
#
.PHONY: test-api
test-api:
test-api: FLAGS ?= -race -shuffle on
test-api: PACKAGES = $(shell cd api && go list ./...)
test-api: $(VERSRC) $(TEST_LOG_DIR) $(RENDER_TESTS)
	$(CGOFLAG) go test -json -tags "$(PAM_TAG) $(FIPS_TAG) $(BPF_TAG)" $(PACKAGES) $(FLAGS) $(ADDFLAGS) \
		| tee $(TEST_LOG_DIR)/api.json \
		| ${RENDER_TESTS}

#
# Runs Teleport Operator tests.
# We have to run them using the makefile to ensure the installation of the k8s test tools (envtest)
#
.PHONY: test-operator
test-operator:
	make -C operator test

#
# Runs cargo test on our Rust modules.
# (a no-op if cargo and rustc are not installed)
#
ifneq ($(CHECK_RUST),)
ifneq ($(CHECK_CARGO),)
.PHONY: test-rust
test-rust:
	cargo test
else
.PHONY: test-rust
test-rust:
endif
endif

# Find and run all shell script unit tests (using https://github.com/bats-core/bats-core)
.PHONY: test-sh
test-sh:
	@if ! type bats 2>&1 >/dev/null; then \
		echo "Not running 'test-sh' target as 'bats' is not installed."; \
		if [ "$${DRONE}" = "true" ]; then echo "This is a failure when running in CI." && exit 1; fi; \
		exit 0; \
	fi; \
	find . -iname "*.bats" -exec dirname {} \; | uniq | xargs -t -L1 bats $(BATSFLAGS)


.PHONY: run-etcd
run-etcd:
	examples/etcd/start-etcd.sh
#
# Integration tests. Need a TTY to work.
# Any tests which need to run as root must be skipped during regular integration testing.
#
.PHONY: integration
integration: FLAGS ?= -v -race
integration: PACKAGES = $(shell go list ./... | grep integration)
integration:  $(TEST_LOG_DIR) $(RENDER_TESTS)
	@echo KUBECONFIG is: $(KUBECONFIG), TEST_KUBE: $(TEST_KUBE)
	$(CGOFLAG) go test -timeout 30m -json -tags "$(PAM_TAG) $(FIPS_TAG) $(BPF_TAG) $(RDPCLIENT_TAG)" $(PACKAGES) $(FLAGS) \
		| tee $(TEST_LOG_DIR)/integration.json \
		| $(RENDER_TESTS) -report-by test

#
# Integration tests which need to be run as root in order to complete successfully
# are run separately to all other integration tests. Need a TTY to work.
#
INTEGRATION_ROOT_REGEX := ^TestRoot
.PHONY: integration-root
integration-root: FLAGS ?= -v -race
integration-root: PACKAGES = $(shell go list ./... | grep integration)
integration-root: $(TEST_LOG_DIR) $(RENDER_TESTS)
	$(CGOFLAG) go test -json -run "$(INTEGRATION_ROOT_REGEX)" $(PACKAGES) $(FLAGS) \
		| tee $(TEST_LOG_DIR)/integration-root.json \
		| $(RENDER_TESTS) -report-by test

#
# Lint the source code.
# By default lint scans the entire repo. Pass GO_LINT_FLAGS='--new' to only scan local
# changes (or last commit).
#
.PHONY: lint
lint: lint-sh lint-helm lint-api lint-go lint-license lint-rust lint-tools

.PHONY: lint-tools
lint-tools: lint-build-tooling lint-bot lint-ci-scripts lint-backport

#
# Runs the clippy linter on our rust modules
# (a no-op if cargo and rustc are not installed)
#
ifneq ($(CHECK_RUST),)
ifneq ($(CHECK_CARGO),)
.PHONY: lint-rust
lint-rust:
	cargo clippy --locked --all-targets -- -D warnings \
		&& cargo fmt -- --check
else
.PHONY: lint-rust
lint-rust:
endif
endif

.PHONY: lint-go
lint-go: GO_LINT_FLAGS ?=
lint-go:
	golangci-lint run -c .golangci.yml --build-tags='$(LIBFIDO2_TEST_TAG) $(TOUCHID_TAG)' $(GO_LINT_FLAGS)

.PHONY: lint-build-tooling
lint-build-tooling: GO_LINT_FLAGS ?=
lint-build-tooling:
	cd build.assets/tooling && golangci-lint run -c ../../.golangci.yml $(GO_LINT_FLAGS)

.PHONY: lint-backport
lint-backport: GO_LINT_FLAGS ?=
lint-backport:
	cd assets/backport && golangci-lint run -c ../../.golangci.yml $(GO_LINT_FLAGS)

.PHONY: lint-bot
lint-bot: GO_LINT_FLAGS ?=
lint-bot:
	cd .github/workflows/robot && golangci-lint run -c ../../../.golangci.yml $(GO_LINT_FLAGS)

.PHONY: lint-ci-scripts
lint-ci-scripts: GO_LINT_FLAGS ?=
lint-ci-scripts:
	cd .cloudbuild/scripts/ && golangci-lint run -c ../../.golangci.yml $(GO_LINT_FLAGS)

# api is no longer part of the teleport package, so golangci-lint skips it by default
.PHONY: lint-api
lint-api: GO_LINT_API_FLAGS ?=
lint-api:
	cd api && golangci-lint run -c ../.golangci.yml $(GO_LINT_API_FLAGS)

# TODO(awly): remove the `--exclude` flag after cleaning up existing scripts
.PHONY: lint-sh
lint-sh: SH_LINT_FLAGS ?=
lint-sh:
	find . -type f -name '*.sh' | xargs \
		shellcheck \
		--exclude=SC2086 \
		$(SH_LINT_FLAGS)

	# lint AWS AMI scripts
	# SC1091 prints errors when "source" directives are not followed
	find assets/aws/files/bin -type f | xargs \
		shellcheck \
		--exclude=SC2086 \
		--exclude=SC1091 \
		--exclude=SC2129 \
		$(SH_LINT_FLAGS)

# Lints all the Helm charts found in directories under examples/chart and exits on failure
# If there is a .lint directory inside, the chart gets linted once for each .yaml file in that directory
# We inherit yamllint's 'relaxed' configuration as it's more compatible with Helm output and will only error on
# show-stopping issues. Kubernetes' YAML parser is not particularly fussy.
# If errors are found, the file is printed with line numbers to aid in debugging.
.PHONY: lint-helm
lint-helm:
	@if ! type yamllint 2>&1 >/dev/null; then \
		echo "Not running 'lint-helm' target as 'yamllint' is not installed."; \
		if [ "$${DRONE}" = "true" ]; then echo "This is a failure when running in CI." && exit 1; fi; \
		exit 0; \
	fi; \
	for CHART in $$(find examples/chart -mindepth 1 -maxdepth 1 -type d); do \
		if [ -d $${CHART}/.lint ]; then \
			for VALUES in $${CHART}/.lint/*.yaml; do \
				export HELM_TEMP=$$(mktemp); \
				echo -n "Using values from '$${VALUES}': "; \
				yamllint -c examples/chart/.lint-config.yaml $${VALUES} || { cat -en $${VALUES}; exit 1; }; \
				helm lint --strict $${CHART} -f $${VALUES} || exit 1; \
				helm template test $${CHART} -f $${VALUES} 1>$${HELM_TEMP} || exit 1; \
				yamllint -c examples/chart/.lint-config.yaml $${HELM_TEMP} || { cat -en $${HELM_TEMP}; exit 1; }; \
			done \
		else \
			export HELM_TEMP=$$(mktemp); \
			helm lint --strict $${CHART} || exit 1; \
			helm template test $${CHART} 1>$${HELM_TEMP} || exit 1; \
			yamllint -c examples/chart/.lint-config.yaml $${HELM_TEMP} || { cat -en $${HELM_TEMP}; exit 1; }; \
		fi; \
	done

ADDLICENSE := $(GOPATH)/bin/addlicense
ADDLICENSE_ARGS := -c 'Gravitational, Inc' -l apache \
		-ignore '**/*.c' \
		-ignore '**/*.h' \
		-ignore '**/*.html' \
		-ignore '**/*.js' \
		-ignore '**/*.py' \
		-ignore '**/*.sh' \
		-ignore '**/*.tf' \
		-ignore '**/*.yaml' \
		-ignore '**/*.yml' \
		-ignore '**/Dockerfile' \
		-ignore 'api/version.go' \
		-ignore 'docs/pages/includes/**/*.go' \
		-ignore 'e/**' \
		-ignore 'gitref.go' \
		-ignore 'lib/srv/desktop/rdp/rdpclient/target/**' \
		-ignore 'lib/teleterm/api/protogen/**' \
		-ignore 'lib/web/build/**' \
		-ignore 'version.go' \
		-ignore 'webassets/**' \
		-ignore 'ignoreme'

.PHONY: lint-license
lint-license: $(ADDLICENSE)
	$(ADDLICENSE) $(ADDLICENSE_ARGS) -check * 2>/dev/null

.PHONY: fix-license
fix-license: $(ADDLICENSE)
	$(ADDLICENSE) $(ADDLICENSE_ARGS) * 2>/dev/null

$(ADDLICENSE):
	cd && go install github.com/google/addlicense@v1.0.0

# This rule triggers re-generation of version files if Makefile changes.
.PHONY: version
version: $(VERSRC)

# This rule triggers re-generation of version files specified if Makefile changes.
$(VERSRC): Makefile
	VERSION=$(VERSION) $(MAKE) -f version.mk setver
	# Update api module path, but don't fail on error.
	$(MAKE) update-api-import-path || true

# This rule updates the api module path to be in sync with the current api release version.
# e.g. github.com/gravitational/teleport/api/vX -> github.com/gravitational/teleport/api/vY
#
# It will immediately fail if:
#  1. A suffix is present in the version - e.g. "v7.0.0-alpha"
#  2. The major version suffix in the api module path hasn't changed. e.g:
#    - v7.0.0 -> v7.1.0 - both use version suffix "/v7" - github.com/gravitational/teleport/api/v7
#    - v0.0.0 -> v1.0.0 - both have no version suffix - github.com/gravitational/teleport/api
#
# Note: any build flags needed to compile go files (such as build tags) should be provided below.
.PHONY: update-api-import-path
update-api-import-path:
	go run build.assets/gomod/update-api-import-path/main.go -tags "bpf fips pam desktop_access_rdp linux"
	$(MAKE) grpc

# make tag - prints a tag to use with git for the current version
# 	To put a new release on Github:
# 		- bump VERSION variable
# 		- run make setver
# 		- commit changes to git
# 		- build binaries with 'make release'
# 		- run `make tag` and use its output to 'git tag' and 'git push --tags'
.PHONY: update-tag
update-tag:
	@test $(VERSION)
	git tag $(GITTAG)
	git tag api/$(GITTAG)
	git push origin $(GITTAG) && git push origin api/$(GITTAG)

# build/webassets directory contains the web assets (UI) which get
# embedded in the teleport binary
$(ASSETS_BUILDDIR)/webassets: ensure-webassets $(ASSETS_BUILDDIR)
ifneq ("$(OS)", "windows")
	@echo "---> Copying OSS web assets."; \
	rm -rf $(ASSETS_BUILDDIR)/webassets; \
	mkdir $(ASSETS_BUILDDIR)/webassets; \
	cd webassets/teleport/ ; cp -r . ../../$@
endif

$(ASSETS_BUILDDIR):
	mkdir -p $@


.PHONY: test-package
test-package: remove-temp-files
	go test -v ./$(p)

.PHONY: test-grep-package
test-grep-package: remove-temp-files
	go test -v ./$(p) -check.f=$(e)

.PHONY: cover-package
cover-package: remove-temp-files
	go test -v ./$(p)  -coverprofile=/tmp/coverage.out
	go tool cover -html=/tmp/coverage.out

.PHONY: profile
profile:
	go tool pprof http://localhost:6060/debug/pprof/profile

.PHONY: sloccount
sloccount:
	find . -o -name "*.go" -print0 | xargs -0 wc -l

.PHONY: remove-temp-files
remove-temp-files:
	find . -name flymake_* -delete

# Dockerized build: useful for making Linux releases on OSX
.PHONY:docker
docker:
	make -C build.assets build

# Dockerized build: useful for making Linux binaries on macOS
.PHONY:docker-binaries
docker-binaries: clean
	make -C build.assets build-binaries

# Interactively enters a Docker container (which you can build and run Teleport inside of)
.PHONY:enter
enter:
	make -C build.assets enter

# Interactively enters a Docker container, as root (which you can build and run Teleport inside of)
.PHONY:enter-root
enter-root:
	make -C build.assets enter-root

# Interactively enters a Docker container (which you can build and run Teleport inside of).
# Similar to `enter`, but uses the centos7 container.
.PHONY:enter/centos7
enter/centos7:
	make -C build.assets enter/centos7

# Interactively enters a Docker container (which you can build and run Teleport Connect inside of).
# Similar to `enter`, but uses the teleterm container.
.PHONY:enter/teleterm
enter/teleterm:
	make -C build.assets enter/teleterm

# grpc generates GRPC stubs from service definitions.
# This target runs in the buildbox container.
.PHONY: grpc
grpc:
	$(MAKE) -C build.assets grpc

# grpc/host generates GRPC stubs.
# Unlike grpc, this target runs locally.
.PHONY: grpc/host
grpc/host:
	@build.assets/genproto.sh

print/env:
	env

# grpc-teleterm generates Go, TypeScript and JavaScript gRPC stubs from definitions for Teleport
# Terminal. This target runs in the buildbox-teleterm container.
#
# It exists as a separate target because on M1 MacBooks we must build grpc_node_plugin from source.
# That involves apt-get install of cmake & build-essential as well pulling hundreds of megabytes of
# git repos. It would significantly increase the time it takes to build buildbox for M1 users that
# don't need to generate Teleterm gRPC files.
# TODO(ravicious): incorporate grpc-teleterm into grpc once grpc-tools adds arm64 binary.
# https://github.com/grpc/grpc-node/issues/1405
.PHONY: grpc-teleterm
grpc-teleterm:
	$(MAKE) -C build.assets grpc-teleterm

# grpc-teleterm/host generates GRPC stubs.
# Unlike grpc-teleterm, this target runs locally.
.PHONY: grpc-teleterm/host
grpc-teleterm/host:
	$(CLANG_FORMAT) -i -style=$(CLANG_FORMAT_STYLE) \
		lib/teleterm/api/proto/**/*.proto

	cd lib/teleterm && buf generate

.PHONY: goinstall
goinstall:
	go install $(BUILDFLAGS) \
		github.com/gravitational/teleport/tool/tsh \
		github.com/gravitational/teleport/tool/teleport \
		github.com/gravitational/teleport/tool/tctl

# make install will installs system-wide teleport
.PHONY: install
install: build
	@echo "\n** Make sure to run 'make install' as root! **\n"
	cp -f $(BUILDDIR)/tctl      $(BINDIR)/
	cp -f $(BUILDDIR)/tsh       $(BINDIR)/
	cp -f $(BUILDDIR)/teleport  $(BINDIR)/
	mkdir -p $(DATADIR)


# Docker image build. Always build the binaries themselves within docker (see
# the "docker" rule) to avoid dependencies on the host libc version.
.PHONY: image
image: clean docker-binaries
	cp ./build.assets/charts/Dockerfile $(BUILDDIR)/
	cd $(BUILDDIR) && docker build --no-cache . -t $(DOCKER_IMAGE_QUAY):$(VERSION)
	if [ -f e/Makefile ]; then $(MAKE) -C e image; fi

.PHONY: publish
publish: image
	docker push $(DOCKER_IMAGE_QUAY):$(VERSION)
	if [ -f e/Makefile ]; then $(MAKE) -C e publish; fi

.PHONY: publish-ecr
publish-ecr: image
	docker tag $(DOCKER_IMAGE_QUAY) $(DOCKER_IMAGE_ECR)
	docker push $(DOCKER_IMAGE_ECR):$(VERSION)
	if [ -f e/Makefile ]; then $(MAKE) -C e publish-ecr; fi

# Docker image build in CI.
# This is run to build and push Docker images to a private repository as part of the build process.
# When we are ready to make the images public after testing (i.e. when publishing a release), we pull these
# images down, retag them and push them up to the production repo so they're available for use.
# This job can be removed/consolidated after we switch over completely from using Jenkins to using Drone.
.PHONY: image-ci
image-ci: clean docker-binaries
	cp ./build.assets/charts/Dockerfile $(BUILDDIR)/
	cd $(BUILDDIR) && docker build --no-cache . -t $(DOCKER_IMAGE_STAGING):$(VERSION)
	if [ -f e/Makefile ]; then $(MAKE) -C e image-ci; fi


# DOCKER_CLI_EXPERIMENTAL=enabled is set to allow inspecting the manifest for present images.
# https://docs.docker.com/engine/reference/commandline/cli/#experimental-features
# The internal staging images use amazon ECR's immutable repository settings. This makes overwrites impossible currently.
# This can cause issues when drone tagging pipelines must be re-run due to failures.
# Currently the work around for this is to not attempt to push to the image when it already exists.
.PHONY: publish-ci
publish-ci: image-ci
	@if DOCKER_CLI_EXPERIMENTAL=enabled docker manifest inspect "$(DOCKER_IMAGE_STAGING):$(VERSION)" >/dev/null 2>&1; then\
		echo "$(DOCKER_IMAGE_STAGING):$(VERSION) already exists. ";     \
	else                                                                \
		docker push "$(DOCKER_IMAGE_STAGING):$(VERSION)";                 \
	fi
	if [ -f e/Makefile ]; then $(MAKE) -C e publish-ci; fi

# Docker image build for Teleport Operator
.PHONY: image-operator-ci
image-operator-ci:
	make -C operator docker-build IMG="$(DOCKER_IMAGE_OPERATOR_STAGING):$(VERSION)"

# DOCKER_CLI_EXPERIMENTAL=enabled is set to allow inspecting the manifest for present images.
# https://docs.docker.com/engine/reference/commandline/cli/#experimental-features
# The internal staging images use amazon ECR's immutable repository settings. This makes overwrites impossible currently.
# This can cause issues when drone tagging pipelines must be re-run due to failures.
# Currently the work around for this is to not attempt to push to the image when it already exists.
.PHONY: publish-operator-ci
publish-operator-ci: image-operator-ci
	@if DOCKER_CLI_EXPERIMENTAL=enabled docker manifest inspect "$(DOCKER_IMAGE_OPERATOR_STAGING):$(VERSION)" >/dev/null 2>&1; then \
		echo "$(DOCKER_IMAGE_OPERATOR_STAGING):$(VERSION) already exists. ";                                                         \
	else                                                                                                                             \
		docker push "$(DOCKER_IMAGE_OPERATOR_STAGING):$(VERSION)";                                                                   \
	fi

.PHONY: print-version
print-version:
	@echo $(VERSION)

.PHONY: chart-ent
chart-ent:
	$(MAKE) -C e chart

RUNTIME_SECTION ?=
TARBALL_PATH_SECTION ?=

ifneq ("$(RUNTIME)", "")
	RUNTIME_SECTION := -r $(RUNTIME)
endif
ifneq ("$(OSS_TARBALL_PATH)", "")
	TARBALL_PATH_SECTION := -s $(OSS_TARBALL_PATH)
endif

# build .pkg
.PHONY: pkg
pkg:
	mkdir -p $(BUILDDIR)/
	cp ./build.assets/build-package.sh ./build.assets/build-common.sh $(BUILDDIR)/
	chmod +x $(BUILDDIR)/build-package.sh
	# arch and runtime are currently ignored on OS X
	# we pass them through for consistency - they will be dropped by the build script
	cd $(BUILDDIR) && ./build-package.sh -t oss -v $(VERSION) -p pkg -a $(ARCH) $(RUNTIME_SECTION) $(TARBALL_PATH_SECTION)
	if [ -f e/Makefile ]; then $(MAKE) -C e pkg; fi

# build tsh client-only .pkg
.PHONY: pkg-tsh
pkg-tsh:
	./build.assets/build-pkg-tsh.sh -t oss -v $(VERSION) $(TARBALL_PATH_SECTION)
	mkdir -p $(BUILDDIR)/
	mv tsh*.pkg* $(BUILDDIR)/

# build .rpm
.PHONY: rpm
rpm:
	mkdir -p $(BUILDDIR)/
	cp ./build.assets/build-package.sh ./build.assets/build-common.sh $(BUILDDIR)/
	chmod +x $(BUILDDIR)/build-package.sh
	cp -a ./build.assets/rpm $(BUILDDIR)/
	cp -a ./build.assets/rpm-sign $(BUILDDIR)/
	cd $(BUILDDIR) && ./build-package.sh -t oss -v $(VERSION) -p rpm -a $(ARCH) $(RUNTIME_SECTION) $(TARBALL_PATH_SECTION)
	if [ -f e/Makefile ]; then $(MAKE) -C e rpm; fi

# build unsigned .rpm (for testing)
.PHONY: rpm-unsigned
rpm-unsigned:
	$(MAKE) UNSIGNED_RPM=true rpm

# build .deb
.PHONY: deb
deb:
	mkdir -p $(BUILDDIR)/
	cp ./build.assets/build-package.sh ./build.assets/build-common.sh $(BUILDDIR)/
	chmod +x $(BUILDDIR)/build-package.sh
	cd $(BUILDDIR) && ./build-package.sh -t oss -v $(VERSION) -p deb -a $(ARCH) $(RUNTIME_SECTION) $(TARBALL_PATH_SECTION)
	if [ -f e/Makefile ]; then $(MAKE) -C e deb; fi

# check binary compatibility with different OSes
.PHONY: test-compat
test-compat:
	./build.assets/build-test-compat.sh

.PHONY: ensure-webassets
ensure-webassets:
	@if [ ! -d $(shell pwd)/webassets/teleport/ ]; then \
		$(MAKE) init-webapps-submodules; \
	fi;

.PHONY: ensure-webassets-e
ensure-webassets-e:
	@if [ ! -d $(shell pwd)/webassets/e/teleport ]; then \
		$(MAKE) init-webapps-submodules-e; \
	fi;

.PHONY: init-webapps-submodules
init-webapps-submodules:
	echo "init webassets submodule"
	git submodule update --init webassets

.PHONY: init-webapps-submodules-e
init-webapps-submodules-e:
	echo "init webassets oss and enterprise submodules"
	git submodule update --init --recursive webassets

.PHONY: init-submodules-e
init-submodules-e: init-webapps-submodules-e
	git submodule init e
	git submodule update

# update-webassets creates a PR in the teleport repo to update webassets submodule.
.PHONY: update-webassets
update-webassets: WEBASSETS_BRANCH ?= 'master'
update-webassets: TELEPORT_BRANCH ?= 'master'
update-webassets:
	build.assets/webapps/update-teleport-webassets.sh -w $(WEBASSETS_BRANCH) -t $(TELEPORT_BRANCH)

# dronegen generates .drone.yml config
#
#    Usage:
#    - install github.com/gravitational/tdr
#    - set $DRONE_TOKEN and $DRONE_SERVER (https://drone.platform.teleport.sh)
#    - tsh login --proxy=platform.teleport.sh
#    - tsh app login drone
#    - make dronegen
.PHONY: dronegen
dronegen:
	go run ./dronegen

# backport will automatically create backports for a given PR as long as you have the "gh" tool
# installed locally. To backport, type "make backport PR=1234 TO=branch/1,branch/2".
.PHONY: backport
backport:
	(cd ./assets/backport && go run main.go -pr=$(PR) -to=$(TO))
