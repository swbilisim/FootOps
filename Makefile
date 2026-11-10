VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/swbilisim/FootOps/internal/version
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

.PHONY: build linux release-build footops-mac mac mac-install test vet check clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o footops ./cmd/footops

# Cross-compile the server binaries into dist/
linux:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/footops_linux_amd64 ./cmd/footops
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/footops_linux_arm64 ./cmd/footops
	cd dist && shasum -a 256 footops_linux_* > SHA256SUMS

# A release's server binaries (panel-tasarim §31.2): the same bytes from the same commit
# on the Mac, which signs them (footops-mac yayin yap), and in CI, which rebuilds the
# published tag and compares. Nothing of the build machine goes in: no paths, no VCS
# stamp, no build ID; the date is the commit's.
RCOMMIT = $(shell git rev-parse --short=7 HEAD)
RDATE   = $(shell TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ)
release-build:
	@case "$(RELEASE)" in v*.*.*) ;; *) echo "RELEASE=vX.Y.Z gerekli" >&2; exit 1 ;; esac
	rm -rf dist/release && mkdir -p dist/release
	for a in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$a GOAMD64=v1 GOARM64=v8.0 GOFLAGS= go build -trimpath -buildvcs=false \
			-ldflags "-s -w -buildid= -X $(PKG).Version=$(RELEASE) -X $(PKG).Commit=$(RCOMMIT) -X $(PKG).Date=$(RDATE)" \
			-o dist/release/footops_linux_$$a ./cmd/footops || exit 1; \
	done

# The Mac's command (panel-tasarim §29) for Apple silicon: dist/footops-mac
footops-mac:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/footops-mac ./cmd/footops-mac

# The Mac app with the core inside (panel-tasarim §29.2): dist/FootOps.app
mac: footops-mac
	mac/app/paketle.sh dist/footops-mac "$(VERSION)" dist

LSREGISTER := /System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister

# Into Applications (every running FootOps quit first: SIGTERM is its Çık), footops-mac on
# the PATH, then opened. The build's copy goes: Spotlight and Finder would open it as a
# second FootOps.
mac-install: mac
	@pkill -x FootOps || true
	@for i in 1 2 3 4 5 6 7 8 9 10; do pgrep -xq FootOps || break; sleep 0.5; done
	@if pgrep -xq FootOps; then echo "FootOps kapanmadı: menüden Çık'ı seçip yeniden deneyin" >&2; exit 1; fi
	rm -rf /Applications/FootOps.app
	cp -R dist/FootOps.app /Applications/FootOps.app
	$(LSREGISTER) -u dist/FootOps.app
	rm -rf dist/FootOps.app
	mkdir -p "$(HOME)/.local/bin"
	ln -sf /Applications/FootOps.app/Contents/MacOS/footops-mac "$(HOME)/.local/bin/footops-mac"
	open /Applications/FootOps.app

test:
	go test -race ./...

vet:
	go vet ./...

check: vet test

clean:
	rm -rf dist footops
