# Build the manager binary
FROM golang:1.16 as builder

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod.int go.mod
COPY go.sum go.sum
COPY pkg/ pkg/

#RUN go mod tidy

# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download -x

# Copy the go source
COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager main.go

FROM registry.access.redhat.com/ubi8/ubi-minimal
RUN microdnf install pciutils kmod procps iproute nc
WORKDIR /
COPY --from=builder /workspace/manager .
COPY assets assets

ENTRYPOINT ["/manager"]
