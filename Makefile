BIN_DIR := ${HOME}/.local/bin


build:
	go build -ldflags "-X main.version=$(shell git describe --tags --always --dirty)" -o ${BIN_DIR}/makeslop ./cmd/makeslop
