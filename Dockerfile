# syntax=docker/dockerfile:1.7
#
# Build stage uses ubi9/go-toolset:9.8-1781070142 which contains Go 1.26.3,
# matching go.mod's `go 1.26.0` directive. The 1.22-* tags only ship Go 1.22
# and cannot consume k8s.io/client-go v0.36.2's prebuilt packages.
FROM registry.access.redhat.com/ubi9/go-toolset:9.8-1781070142 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY web/ web/
COPY scripts/ scripts/
RUN ./scripts/fetch-htmx.sh && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/helper ./cmd/helper

FROM registry.access.redhat.com/ubi9/ubi-minimal:9.8
RUN microdnf install -y --setopt=tsflags=nodocs ca-certificates && \
    microdnf clean all

RUN useradd -u 1001 -r -g 0 helper
WORKDIR /app
COPY --from=build /out/helper /app/helper
COPY --from=build /src/web/static/ /app/web/static/
COPY --from=build /src/web/templates/ /app/web/templates/

RUN mkdir -p /app/.cache /tmp && \
    chown -R 1001:0 /app && \
    chmod -R g+w /app /tmp

USER 1001
EXPOSE 8080
ENTRYPOINT ["/app/helper"]
