FROM docker.io/library/golang:1.26 AS builder

WORKDIR /go/src/github.com/wzshiming/kube-activator

RUN --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -o activator ./cmd/activator

FROM docker.io/library/alpine:3.22

COPY --from=builder /go/src/github.com/wzshiming/kube-activator/activator /usr/local/bin/activator

ENTRYPOINT ["/usr/local/bin/activator"]
