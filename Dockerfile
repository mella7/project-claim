# Build on the machine's own architecture and cross-compile for the target.
# That keeps multi-arch builds fast, because nothing runs under emulation.
FROM --platform=$BUILDPLATFORM golang:1.24 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace

# Dependencies first, so this layer is reused until go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

# CGO off gives a static binary that needs nothing from the base image.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o manager ./cmd

# distroless/static has no shell and no package manager. The numeric non-root
# user lets Kubernetes verify runAsNonRoot; OpenShift replaces it with a UID
# from the namespace's range.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
