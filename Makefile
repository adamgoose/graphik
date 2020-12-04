version := "0.0.33"

.DEFAULT_GOAL := help

.PHONY: help
help:
	@echo "Makefile Commands:"
	@echo "----------------------------------------------------------------"
	@fgrep -h "##" $(MAKEFILE_LIST) | fgrep -v fgrep | sed -e 's/\\$$//' | sed -e 's/##//'
	@echo "----------------------------------------------------------------"

run:
	@go run main.go --open-id https://accounts.google.com/.well-known/openid-configuration

gen: proto gql

patch: ## bump version by 1 patch
	bumpversion patch --allow-dirty

tag: ## tag the repo (remember to commit changes beforehand)
	git tag v$(version)

push:
	git push origin v$(version)

docker-build:
	@docker build -t colemanword/graphik:v$(version) .

docker-push:
	@docker push colemanword/graphik:v$(version)

.PHONY: proto
proto: ## regenerate gRPC code
	@echo "generating protobuf code..."
	@docker run -v `pwd`:/defs namely/prototool:latest generate
	@go fmt ./...

.PHONY: gql
gql: ## regenerate graphql code
	@gqlgen generate