PKG=github.com/cyverse/irodsfsd
VERSION=v0.1.0
GIT_COMMIT?=$(shell git rev-parse HEAD)
BUILD_DATE?=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS?="-X '${PKG}/commons.serviceVersion=${VERSION}' -X '${PKG}/commons.gitCommit=${GIT_COMMIT}' -X '${PKG}/commons.buildDate=${BUILD_DATE}'"
GO111MODULE=on
GOPROXY=direct
GOPATH=$(shell go env GOPATH)
.EXPORT_ALL_VARIABLES:

.PHONY: build
build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux go build -ldflags=${LDFLAGS} -o bin/irodsfsd ./cmd/

.PHONY: protobuf
protobuf:
# This requires installation of two modules to compile protobuf and grpc
# sudo apt install protobuf-compiler
# go get google.golang.org/grpc/cmd/protoc-gen-go-grpc
# go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.28
	export PATH=${PATH}:${GOPATH}/bin; protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative service/api/api.proto


.PHONY: examples
examples:
	CGO_ENABLED=0 GOOS=linux go build -ldflags=${LDFLAGS} -o ./client_examples/mount/mount.out ./client_examples/mount/mount.go
	CGO_ENABLED=0 GOOS=linux go build -ldflags=${LDFLAGS} -o ./client_examples/unmount/unmount.out ./client_examples/unmount/unmount.go
	CGO_ENABLED=0 GOOS=linux go build -ldflags=${LDFLAGS} -o ./client_examples/mount_list/mount_list.out ./client_examples/mount_list/mount_list.go
