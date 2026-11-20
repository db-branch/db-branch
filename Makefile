GOARCH  ?= $(shell go env GOARCH)
# Tagged builds use the tag; development builds sort by build time so a newer
# .deb always upgrades an older one. Computed once per make run (:=), so every
# package of one build shares it.
ifndef VERSION
VERSION := $(shell git describe --tags --exact-match --match 'v*' 2>/dev/null | sed 's/^v//' | grep . || echo "0.0.0~dev$$(date -u +%Y%m%d%H%M%S).$$(git rev-parse --short HEAD)")
endif
LDFLAGS := -s -w -X main.version=$(VERSION)
COVER_MIN := 95

.PHONY: all build test cover vet integration deb deb-oss deb-proxysql deb-greenmask deb-greenmask-mysql deb-anon apt-repo clean web web-test demo

all: vet cover build

# Builds the console into web/dist, which build embeds. Needs Node.js.
web:
	cd web && npm ci --no-audit --no-fund && npm run build
	touch web/dist/.gitkeep

web-test:
	cd web && npm ci --no-audit --no-fund && npm test

# The static demo console (fake agent in the browser, for demo.db-branch.com) -> web/dist-demo. See web/demo-worker.
demo:
	cd web && npm ci --no-audit --no-fund && npm run build:demo

# TAGS=dev builds in the development licence root key (test licences only).
TAGS ?=

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -tags '$(TAGS)' -ldflags '$(LDFLAGS)' \
		-o bin/db-branch ./cmd/db-branch

test:
	go test ./...

vet:
	go vet ./...

# Fails if any package under internal/ is below $(COVER_MIN)% coverage.
# internal/app is the composition root (like cmd/): `make integration` exercises it.
# pkg/ext/proto holds generated code (make proto); the packages that use it test its behavior.
COVER_PKGS = $(shell go list ./internal/... ./pkg/... | grep -v -e '/internal/app$$' -e '/pkg/ext/proto/')
cover:
	@go test -cover $(COVER_PKGS) | tee /dev/stderr | awk -v min=$(COVER_MIN) ' \
		/coverage:/ { for (i = 1; i <= NF; i++) if ($$i == "coverage:") { pct = $$(i+1); sub("%", "", pct); \
			if (pct + 0 < min) { print "coverage below " min "%: " $$2 " (" pct "%)"; bad = 1 } } } \
		/^FAIL/ { bad = 1 } \
		END { exit bad }'

# Touches a real ZFS pool (default: tank), systemd and mysqld. Run as root on a dev VM
# with the package installed (make deb && apt install ./dist/*.deb).
DB_BRANCH_TEST_POOL ?= tank
integration:
	DB_BRANCH_TEST_POOL=$(DB_BRANCH_TEST_POOL) go test -count=1 -v -tags integration ./internal/infrastructure/zfs/ ./test/integration/

# Regenerates pkg/ext/proto/**/*.pb.go. Needs protoc and, in $(GOPATH)/bin,
# protoc-gen-go (google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12) and
# protoc-gen-go-grpc (google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2).
proto:
	cd pkg/ext/proto/runtime/v1 && PATH="$$PATH:$$(go env GOPATH)/bin" protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative runtime.proto

deb: web build
	@mkdir -p dist
	GOARCH=$(GOARCH) VERSION=$(VERSION) nfpm package --packager deb --config packaging/nfpm.yaml --target dist/

# The ProxySQL module (and its dependency on the pinned ProxySQL).
deb-greenmask:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' \
		-o bin/db-branch-greenmask ./cmd/db-branch-greenmask
	GOARCH=$(GOARCH) VERSION=$(VERSION) nfpm package --packager deb --config packaging/greenmask/nfpm.yaml --target dist/

deb-greenmask-mysql:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' \
		-o bin/db-branch-greenmask-mysql ./cmd/db-branch-greenmask-mysql
	GOARCH=$(GOARCH) VERSION=$(VERSION) nfpm package --packager deb --config packaging/greenmask-mysql/nfpm.yaml --target dist/

deb-anon:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' \
		-o bin/db-branch-anon ./cmd/db-branch-anon
	GOARCH=$(GOARCH) VERSION=$(VERSION) nfpm package --packager deb --config packaging/anon/nfpm.yaml --target dist/

deb-proxysql:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' \
		-o bin/db-branch-proxysql ./cmd/db-branch-proxysql
	. packaging/proxysql/upstream.env && GOARCH=$(GOARCH) VERSION=$(VERSION) PROXYSQL_VERSION=$$PROXYSQL_VERSION \
		nfpm package --packager deb --config packaging/proxysql/nfpm.yaml --target dist/

# A signed APT repository in dist/apt from dist/*.deb plus the mirrored
# ProxySQL release. APT_KEY is the signing key's id in gpg's keyring.
# Every package of the free (Apache-2.0) edition, for one architecture.
deb-oss: deb deb-proxysql deb-greenmask deb-greenmask-mysql deb-anon

apt-repo:
	test -n "$(APT_KEY)" || { echo "set APT_KEY to the signing key id"; exit 1; }
	scripts/fetch-proxysql.sh dist/upstream
	scripts/apt-repo.sh dist/apt $(APT_KEY) dist/*.deb dist/upstream/*

clean:
	go clean -testcache
	-rm -r bin dist
