# Build a small static binary for the target platform (ARM64 / AMD64).
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder
WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
COPY main.go .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/kuma-docker-recovery ./main.go

FROM scratch
COPY --from=builder /out/kuma-docker-recovery /kuma-docker-recovery
EXPOSE 8080
ENTRYPOINT ["/kuma-docker-recovery"]
