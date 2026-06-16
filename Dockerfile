# syntax=docker/dockerfile:1.7
#
# Build stage uses ubi9/go-toolset:9.8-1781070142 which contains Go 1.26.3,
# matching go.mod's `go 1.26.0` directive. The 1.22-* tags only ship Go 1.22
# and cannot consume k8s.io/client-go v0.36.2's prebuilt packages.
#
# USER root: the go-toolset image's default user (uid 1001) cannot write
# into the COPY'd web/ tree during fetch-htmx.sh (curl exits 23). Switching
# to root for the build stage fixes it; the runtime stage already drops
# privileges to uid 1001.
# VERSION is injected by the CI workflow from the git tag (e.g. v0.3.1)
# on tag builds, and defaults to "dev" for local builds. Declared at
# the top of the file so it spans both build and runtime stages; an
# in-stage ARG would be invisible to the second FROM.
ARG VERSION=dev

FROM registry.access.redhat.com/ubi9/go-toolset:9.8-1781070142 AS build
USER root
WORKDIR /src

COPY --chown=0:0 go.mod go.sum ./
RUN go mod download

COPY --chown=0:0 cmd/ cmd/
COPY --chown=0:0 internal/ internal/
COPY --chown=0:0 web/ web/
COPY --chown=0:0 scripts/ scripts/
# Re-declare ARG after FROM to make $VERSION visible in this stage.
ARG VERSION=dev
# Two outlets for the version:
#   1. -ldflags stamps it into the binary (UI footer reads it)
#   2. ENV exports it (operators can `oc set env` to override, and
#      `kubectl describe pod` shows the value)
ENV VERSION=${VERSION}
RUN ./scripts/fetch-htmx.sh && \
    CGO_ENABLED=0 go build -trimpath -buildvcs=false \
        -ldflags="-s -w -X 'main.version=${VERSION}'" \
        -o /out/helper ./cmd/helper

FROM registry.access.redhat.com/ubi9/ubi-minimal:9.8
# Carry VERSION through the stage boundary so the runtime image
# contains the same value the binary was compiled with. Without
# re-declaring + re-assigning here, runtime ENV would be unset.
ARG VERSION=dev
ENV VERSION=${VERSION}
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
