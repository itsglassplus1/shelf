# Build locally:  podman build -t localhost/shelf:latest .
# Published images are built by .github/workflows/publish.yml.
#
# The build stage always runs on the build machine's own architecture and
# cross-compiles for the target (Go needs no emulation for that), so the
# arm64 image builds as fast as the amd64 one.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY *.go ./
# A failing test stops the build, so a broken change is never published or
# replaces the running image.
RUN CGO_ENABLED=0 go test ./...
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /shelf .

# Final image: just the static binary.
FROM scratch
COPY --from=build /shelf /shelf
EXPOSE 25600
ENTRYPOINT ["/shelf"]
