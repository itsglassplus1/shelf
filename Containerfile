# Build locally:  podman build -t localhost/shelf:latest .
# Published images are built by .github/workflows/publish.yml.
#
# The build stage always runs on the build machine's own architecture and
# cross-compiles for the target (Go needs no emulation for that), so the
# arm64 image builds as fast as the amd64 one.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY *.go ./
# A failing test stops the build, so a broken change is never published or
# replaces the running image.
RUN CGO_ENABLED=0 go test ./...
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /shelf .
# The data folder, owned by the user shelf runs as. A new volume mounted
# there takes this owner.
RUN mkdir -p /rootfs/data

# Final image: just the static binary, run as an unprivileged user
# (1000, usually the host's first user, so bind-mounted files are theirs;
# scratch has no user database, so it is just a number).
FROM scratch
COPY --from=build /shelf /shelf
COPY --from=build --chown=1000:1000 /rootfs/ /
USER 1000:1000
EXPOSE 25600
ENTRYPOINT ["/shelf"]
