FROM golang:1.23-alpine AS builder

RUN apk update && apk add --no-cache git make

WORKDIR /usr/src/app
COPY . .

RUN make build

FROM ubuntu:22.04

# Disable Prompt During Packages Installation
ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends sudo wget \
    net-tools ca-certificates && \
    rm -rf /var/lib/apt/lists/* && apt clean

RUN apk update && apk add --no-cache ca-certificates

ARG GIT_VERSION

LABEL name="kube-loxilb" \
      vendor="loxilb.io" \
      version=${GIT_VERSION:-unknown} \
      release="0.1" \
      summary="kube-loxilb docker image" \
      description="service-lb implementation for loxilb" \
      maintainer="backguyn@netlox.io"

WORKDIR /bin/
COPY --from=builder /usr/src/app/bin/kube-loxilb /bin/kube-loxilb

USER root
RUN chmod +x /bin/kube-loxilb
