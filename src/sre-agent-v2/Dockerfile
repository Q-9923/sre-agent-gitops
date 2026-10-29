FROM golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY internal/ ./internal/

RUN CGO_ENABLED=0 \
    GOMAXPROCS=2 \
    GOOS=linux \
    GOARCH=amd64 \
    go build \
    -p 1 \
    -mod=readonly \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/sre-agent \
    .

FROM scratch

COPY --from=builder \
    /etc/ssl/certs/ca-certificates.crt \
    /etc/ssl/certs/ca-certificates.crt

COPY --from=builder /out/sre-agent /sre-agent

USER 65532:65532

ENTRYPOINT ["/sre-agent"]
