# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/sab2torbox ./cmd/sab2torbox

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sab2torbox /sab2torbox
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=60s --timeout=5s --retries=3 \
    CMD ["/sab2torbox", "healthcheck"]
ENTRYPOINT ["/sab2torbox"]
