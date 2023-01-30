.DEFAULT_GOAL := build
bin=kube-loxilb
tag?=latest

build:
	@mkdir -p ./bin
	go build -o ./bin/${bin} ./cmd/loxilb-agent

clean:
	go clean ./cmd

docker: build
	docker build -t ghcr.io/loxilb-io/${bin}:${tag} .
