# Thin Lambda image over the pinned upstream webmail image: adds Lambda Web
# Adapter as an extension and points the Next.js cache at /tmp, the only
# writable path in Lambda. Build with make webmail-image, which passes the
# digests from versions.env; there are no floating defaults.
ARG UPSTREAM_IMAGE
ARG ADAPTER_IMAGE

FROM ${ADAPTER_IMAGE} AS adapter

FROM ${UPSTREAM_IMAGE}
COPY --from=adapter /lambda-adapter /opt/extensions/lambda-adapter
USER root
RUN rm -rf /app/.next/cache && ln -s /tmp/cache /app/.next/cache
USER nextjs
