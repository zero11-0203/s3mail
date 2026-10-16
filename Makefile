# Local checks to run before every push to main (trunk-based workflow).
# CI runs the same checks in .github/workflows/checks.yml.

GOLANGCI_LINT ?= golangci-lint
TF_DIRS := infra/bootstrap infra/main
TF_TEST_DIRS := $(patsubst %/tests,%,$(wildcard infra/modules/*/tests))

.PHONY: check go-check go-fmt go-vet go-lint go-test go-vuln tf-check secrets spike-build

check: go-check tf-check secrets

go-check: go-fmt go-vet go-lint go-test go-vuln

go-fmt:
	@unformatted="$$(cd go && gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

go-vet:
	cd go && go vet ./...

go-lint:
	cd go && $(GOLANGCI_LINT) run ./...

go-test:
	cd go && go test -race ./...

go-vuln:
	cd go && go tool govulncheck ./...

tf-check:
	terraform fmt -check -recursive infra
	@for d in $(TF_DIRS); do \
		echo "terraform validate $$d"; \
		terraform -chdir=$$d init -backend=false -input=false >/dev/null && \
		terraform -chdir=$$d validate || exit 1; \
	done
	@for d in $(TF_TEST_DIRS); do \
		echo "terraform test $$d"; \
		terraform -chdir=$$d init -backend=false -input=false >/dev/null && \
		terraform -chdir=$$d test || exit 1; \
	done

secrets:
	gitleaks git --redact --no-banner .

# Phase 0 streaming spike Lambdas (spike function and bearer-token
# authorizer): linux/arm64 bootstrap binaries, zipped with a fixed timestamp
# so unchanged code gives an unchanged zip hash.
SPIKE_LAMBDAS := spike-stream authorizer

spike-build:
	@set -e; for f in $(SPIKE_LAMBDAS); do \
		echo "build $$f"; \
		mkdir -p go/build/$$f; \
		(cd go && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -trimpath \
			-ldflags "-s -w -X github.com/zero11-0203/s3mail/go/internal/buildinfo.Version=$$(git rev-parse --short HEAD)" \
			-o build/$$f/bootstrap ./cmd/$$f); \
		touch -t 202601010000 go/build/$$f/bootstrap; \
		(cd go/build/$$f && rm -f bootstrap.zip && zip -X -q bootstrap.zip bootstrap); \
	done
