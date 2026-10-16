# Local checks to run before every push to main (trunk-based workflow).
# CI runs the same checks in .github/workflows/checks.yml.

GOLANGCI_LINT ?= golangci-lint
TF_DIRS := infra/bootstrap infra/main

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

secrets:
	gitleaks git --redact --no-banner .

# Phase 0 streaming spike Lambda: linux/arm64 bootstrap binary, zipped with a
# fixed timestamp so unchanged code gives an unchanged zip hash.
SPIKE_BUILD := go/build/spike-stream

spike-build:
	mkdir -p $(SPIKE_BUILD)
	cd go && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -trimpath \
		-ldflags "-s -w -X github.com/zero11-0203/s3mail/go/internal/buildinfo.Version=$$(git rev-parse --short HEAD)" \
		-o build/spike-stream/bootstrap ./cmd/spike-stream
	touch -t 202601010000 $(SPIKE_BUILD)/bootstrap
	cd $(SPIKE_BUILD) && rm -f bootstrap.zip && zip -X -q bootstrap.zip bootstrap
