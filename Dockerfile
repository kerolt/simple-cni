# Build the manager binary
FROM golang:alpine as builder

WORKDIR /workspace
# Copy the Go Modules manifests
COPY ./ ./

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -mod=vendor -o bin/simple-cnid cmd/cnid/main.go && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -mod=vendor -o bin/simple-cni cmd/cni/main.go

FROM alpine
# Need nft for nftables mode, iptables for legacy mode
RUN apk update && apk add --no-cache iptables
WORKDIR /
COPY --from=builder /workspace/bin/* /