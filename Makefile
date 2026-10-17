# Local checks to run before every push to main (trunk-based workflow).
# CI runs the same checks in .github/workflows/checks.yml.

GOLANGCI_LINT ?= golangci-lint
TF_DIRS := infra/bootstrap infra/main infra/webmail
TF_TEST_DIRS := $(patsubst %/tests,%,$(wildcard infra/modules/*/tests))

.PHONY: check go-check go-fmt go-vet go-lint go-test go-vuln tf-check secrets build spike-build webmail-image

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

# Lambda zips: linux/arm64 bootstrap binaries, zipped with a fixed
# timestamp so unchanged code gives an unchanged zip hash.
LAMBDAS := ingest authorizer jmap-api jmap-download jmap-push search-indexer search outbox-dispatcher submission-worker submission-recovery relay-webhook delivery-worker gc
# Phase 0 streaming spike (spike function and bearer-token authorizer).
SPIKE_LAMBDAS := spike-stream authorizer

build: $(addprefix lambda-,$(LAMBDAS))

spike-build: $(addprefix lambda-,$(SPIKE_LAMBDAS))

lambda-%:
	@echo "build $*"
	@mkdir -p go/build/$*
	@cd go && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -trimpath \
		-ldflags "-s -w -X github.com/zero11-0203/s3mail/go/internal/buildinfo.Version=$$(git rev-parse --short HEAD)" \
		-o build/$*/bootstrap ./cmd/$*
	@touch -t 202601010000 go/build/$*/bootstrap
	@cd go/build/$* && rm -f bootstrap.zip && zip -X -q bootstrap.zip bootstrap

# Phase 4 webmail image (docs/operations/deployment.md#webmail-deployment):
# linux/arm64 as a single manifest (Lambda rejects attestation indexes),
# pushed to the infra/webmail repository under an immutable commit tag.
# Prints the digest to set as image_digest in infra/webmail.
webmail-image:
	@test -n "$(WEBMAIL_REPO)" || { echo 'set WEBMAIL_REPO to: terraform -chdir=infra/webmail output -raw repository_url'; exit 1; }
	@set -e; . ./webmail/versions.env; \
	registry="$(firstword $(subst /, ,$(WEBMAIL_REPO)))"; region="$$(echo "$$registry" | cut -d. -f4)"; \
	tag="$(WEBMAIL_REPO):$$(git rev-parse --short HEAD)-$$UPSTREAM_VERSION"; \
	aws ecr get-login-password --region "$$region" | docker login --username AWS --password-stdin "$$registry" >/dev/null; \
	docker buildx build --platform linux/arm64 --provenance=false --sbom=false \
		--build-arg UPSTREAM_IMAGE="$$UPSTREAM_IMAGE" --build-arg ADAPTER_IMAGE="$$ADAPTER_IMAGE" \
		--metadata-file webmail/.build-metadata.json -t "$$tag" --push webmail; \
	digest="$$(grep -o '"containerimage.digest": *"sha256:[0-9a-f]*"' webmail/.build-metadata.json | grep -o 'sha256:[0-9a-f]*')"; \
	rm -f webmail/.build-metadata.json; \
	echo "image_digest = \"$$digest\""
