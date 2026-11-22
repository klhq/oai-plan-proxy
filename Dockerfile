# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH
# Release builds pass the tag and commit. Local builds report "(devel)".
ARG VERSION COMMIT
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=$(echo "${COMMIT}" | cut -c1-12)" \
      -o /out/oai-plan-proxy ./cmd/oai-plan-proxy
# The data directory must belong to the nonroot user before a named volume
# is created from it, so the proxy can write credentials.json.
RUN mkdir -p /out/data && chown 65532:65532 /out/data && chmod 0700 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/oai-plan-proxy /oai-plan-proxy
COPY --from=build --chown=65532:65532 /out/data /data
ENV OAI_PLAN_PROXY_DATA_DIR=/data \
    OAI_PLAN_PROXY_LISTEN=0.0.0.0:18080
VOLUME ["/data"]
EXPOSE 18080
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s \
    CMD ["/oai-plan-proxy", "healthcheck"]
ENTRYPOINT ["/oai-plan-proxy"]
CMD ["serve"]
