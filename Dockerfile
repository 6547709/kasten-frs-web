# syntax=docker/dockerfile:1.7
#
# NOTE: The build stage below uses registry.access.redhat.com/ubi9/go-toolset:1.22-9.4.
# go.mod declares `go 1.26.0`, so the build will fail until this tag is bumped to
# a 1.26-series go-toolset (e.g. `.../go-toolset:1.26-9.4`) or to `:latest`. The
# final build/sweep is in Task 23; if the chosen tag is unavailable, update this
# line. We keep the spec value here so the diff matches the plan.
FROM registry.access.redhat.com/ubi9/go-toolset:1.22-9.4 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY web/ web/
COPY scripts/ scripts/
RUN ./scripts/fetch-htmx.sh && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/helper ./cmd/helper

FROM registry.access.redhat.com/ubi9/ubi-minimal:9.4
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
