# Local checks to run before every push to main (trunk-based workflow).
# CI runs the same checks in .github/workflows/checks.yml.

GOLANGCI_LINT ?= golangci-lint
TF_DIRS := infra/bootstrap infra/main

.PHONY: check go-check go-fmt go-vet go-lint go-test go-vuln tf-check secrets

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
