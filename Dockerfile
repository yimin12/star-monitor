# Builds on the build machine's platform and cross-compiles for each target (amd64, arm64).
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/star-monitor . && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/star-monitor /star-monitor
# An empty /data owned by the runtime user, so a fresh volume is writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV DB_PATH=/data/star-monitor.db LISTEN=:8080
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/star-monitor"]
