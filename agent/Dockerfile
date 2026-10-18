# AgentCore Runtime image (docs/specs/agent.md#deployment-and-operations):
# linux/arm64, serving /invocations and /ping on port 8080. Dependencies come
# only from uv.lock; both base images are pinned by digest. Build and push
# with make agent-image, which prints the digest for infra/agent.
FROM ghcr.io/astral-sh/uv:0.9.5@sha256:f459f6f73a8c4ef5d69f4e6fbbdb8af751d6fa40ec34b39a1ab469acd6e289b7 AS uv

FROM public.ecr.aws/docker/library/python:3.12-slim-bookworm@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258 AS build
COPY --from=uv /uv /usr/local/bin/uv
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never
WORKDIR /app
COPY pyproject.toml uv.lock README.md ./
RUN uv sync --frozen --no-dev --no-install-project
COPY src ./src
RUN uv sync --frozen --no-dev --no-editable

FROM public.ecr.aws/docker/library/python:3.12-slim-bookworm@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258
RUN useradd --uid 10001 --no-create-home agent
COPY --from=build /app/.venv /app/.venv
ENV PATH=/app/.venv/bin:$PATH PYTHONUNBUFFERED=1
USER agent
EXPOSE 8080
# ADOT sends traces to AgentCore Observability; infra/agent turns its
# framework instrumentors and message capture off (no mail in telemetry).
CMD ["opentelemetry-instrument", "python", "-m", "s3mail_agent.app"]
