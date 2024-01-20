FROM golang:1.21

LABEL name="kube-loxilb" \
      vendor="loxilb.io" \
      version=$GIT_VERSION \
      release="0.1" \
      summary="kube-loxilb docker image" \
      description="service-lb implementation for loxilb" \
      maintainer="backguyn@netlox.io"

WORKDIR /bin/
COPY ./bin/kube-loxilb /bin/kube-loxilb
USER root
RUN chmod +x /bin/kube-loxilb
