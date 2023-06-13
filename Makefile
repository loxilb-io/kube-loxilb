.DEFAULT_GOAL := build
bin=kube-loxilb
TAG?=latest

build:
	@mkdir -p ./bin
	go build -o ./bin/${bin} ./cmd/loxilb-agent

clean:
	go clean ./cmd

docker: build
	docker build -t ghcr.io/loxilb-io/${bin}:${TAG} .

docker-rhel: build
	docker build -t ghcr.io/loxilb-io/${bin}-ubi8:${TAG} -f Dockerfile.RHEL .
