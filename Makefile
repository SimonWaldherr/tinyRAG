SHELL := /bin/sh

APP_ADDR := :8080
APP_DB := data/tinyrag.gob
APP_BIN := bin/tinyrag
APP_PKG := ./cmd/tinyrag

.PHONY: fmt vet lint tidy build test check run dev help

fmt:
	gofmt -w $$(find ./cmd ./internal -type f -name '*.go')

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed. Install with 'brew install golangci-lint' or see https://golangci-lint.run/usage/install/."; exit 1; }
	@golangci-lint run ./...

tidy:
	go mod tidy

build:
	mkdir -p bin
	go build -o $(APP_BIN) $(APP_PKG)

test:
	go test ./...

check: fmt vet lint test

run:
	go run $(APP_PKG) -web -addr $(APP_ADDR) -db $(APP_DB)

dev: fmt vet run

help:
	@echo "fmt vet lint tidy build test check run dev help"
