FROM golang:1.23-alpine AS builder

RUN apk update && apk add git && apk add make

WORKDIR /usr/src/app
COPY . .

RUN make build

FROM golang:1.23

LABEL name="kube-loxilb" \
      vendor="loxilb.io" \
      version=$GIT_VERSION \
      release="0.1" \
      summary="kube-loxilb docker image" \
      description="service-lb implementation for loxilb" \
      maintainer="backguyn@netlox.io"

WORKDIR /bin/
COPY --from=builder /usr/src/app/bin/kube-loxilb /bin/kube-loxilb

USER root
RUN chmod +x /bin/kube-loxilb
