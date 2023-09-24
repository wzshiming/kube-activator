FROM docker.io/library/golang:1.21 AS builder

WORKDIR /go/src/github.com/wzshiming/kube-activator

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o activator ./cmd/activator

FROM docker.io/library/alpine:3.18

COPY --from=builder /go/src/github.com/wzshiming/kube-activator/activator /usr/local/bin/activator

ENTRYPOINT ["/usr/local/bin/activator"]
