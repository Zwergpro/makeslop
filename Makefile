BIN_DIR := ${HOME}/.local/bin


build:
	go build -o ${BIN_DIR}/makeslop cmd/makeslop/main.go
