.DEFAULT_GOAL := build
bin=kube-loxilb
TAG?=latest

build:
	@mkdir -p ./bin
	go build -o ./bin/${bin} -ldflags="-X 'main.BuildInfo=${shell date '+%Y_%m_%d'}-${shell git branch --show-current}-$(shell git show --pretty=format:%h --no-patch)' -X 'loxicmd/cmd.Version=${shell git describe --tags --abbrev=0}'" ./cmd/loxilb-agent

clean:
	go clean ./cmd

docker: build
	docker build -t ghcr.io/loxilb-io/${bin}:${TAG} .

docker-rhel: build
	docker build -t ghcr.io/loxilb-io/${bin}-ubi8:${TAG} -f Dockerfile.RHEL .
