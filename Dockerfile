# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
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

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
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
