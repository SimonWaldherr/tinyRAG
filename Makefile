SHELL := /bin/sh

APP_ADDR := :8080
APP_DB := tinyrag.gob

.PHONY: fmt vet lint tidy build test check run dev help

fmt:
	gofmt -w ./*.go

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed. Install with 'brew install golangci-lint' or see https://golangci-lint.run/usage/install/."; exit 1; }
	@golangci-lint run ./...

tidy:
	go mod tidy

build:
	go build ./...

test:
	go test ./...

check: fmt vet lint test

run:
	go run . -web -addr $(APP_ADDR) -db $(APP_DB)

dev: fmt vet run

help:
	@echo "fmt vet lint tidy build test check run dev help"
