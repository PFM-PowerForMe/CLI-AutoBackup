# 构建时
FROM docker.io/library/golang:alpine AS builder
ARG REPO
# eg. amd64 | arm64
ARG ARCH
# eg. x86_64 | aarch64
ARG CPU_ARCH
ARG TAG
# eg. latest
ARG IMAGE_VERSION
ENV REPO=$REPO \
     ARCH=$ARCH \
     CPU_ARCH=$CPU_ARCH \
     TAG=$TAG \
     IMAGE_VERSION=$IMAGE_VERSION

ENV CGO_ENABLED=0 \
     GOOS=linux \
     GOARCH=$ARCH

WORKDIR /output/
WORKDIR /source/
COPY source-src/src/ .
RUN go mod download
RUN go build -o /output/autobackup -trimpath -ldflags="-w -s" main.go


# 运行时
FROM scratch AS runtime
COPY --from=builder /output/autobackup /usr/bin/autobackup
