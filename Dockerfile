FROM eyes852/ubuntu-iperf-test:0.5

LABEL name="kube-loxilb" \
      vendor="loxilb.io" \
      version=$GIT_VERSION \
      release="0.1" \
      summary="loxilb daemonset for kubernetes" \
      description="loxilb daemonSet" \
      maintainer="backguyn@netlox.io"

COPY ./bin/kube-loxilb /bin/kube-loxilb
USER root
RUN chmod +x /bin/kube-loxilb
