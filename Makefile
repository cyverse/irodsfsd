PKG=github.com/cyverse/irodsfsd
VERSION=v0.1.0
GIT_COMMIT?=$(shell git rev-parse HEAD)
BUILD_DATE?=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS?="-X '${PKG}/commons.serviceVersion=${VERSION}' -X '${PKG}/commons.gitCommit=${GIT_COMMIT}' -X '${PKG}/commons.buildDate=${BUILD_DATE}'"
GO111MODULE=on
GOPROXY=direct
GOPATH=$(shell go env GOPATH)
OS_NAME:=$(shell grep -E '^ID=' /etc/os-release | cut -d'=' -f2 | tr -d '"')
SHELL:=/bin/bash
ADDUSER_FLAGS:=
ifeq (${OS_NAME},centos)
	ADDUSER_FLAGS=-r -d /dev/null -s /sbin/nologin
else ifeq (${OS_NAME},almalinux)
	ADDUSER_FLAGS=-r -d /dev/null -s /sbin/nologin
else ifeq (${OS_NAME},ubuntu)
	ADDUSER_FLAGS=--system --no-create-home --shell /sbin/nologin --group
else
	ADDUSER_FLAGS=--system --no-create-home --shell /sbin/nologin --group
endif

.EXPORT_ALL_VARIABLES:

.PHONY: build
build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux go build -ldflags=$(LDFLAGS) -o bin/irodsfsd ./cmd/

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


.PHONY: release
release: build
	mkdir -p release
	mkdir -p release/bin
	cp bin/irodsfsd release/bin
	mkdir -p release/packaging/systemd
	cp packaging/systemd/config.yaml release/packaging/systemd
	cp packaging/systemd/irodsfsd.service release/packaging/systemd
	cp packaging/systemd/README.md release/packaging/systemd
	cp Makefile.release release/Makefile
	cd release && tar zcvf ../irodsfsd.tar.gz *

.PHONY: install
install:
	cp bin/irodsfsd /usr/bin
	cp packaging/systemd/irodsfsd.service /usr/lib/systemd/system/
	id -u irodsfsd &> /dev/null || adduser ${ADDUSER_FLAGS} irodsfsd
	mkdir -p /etc/irodsfsd
	cp packaging/systemd/config.yaml /etc/irodsfsd
	chown irodsfsd:irodsfsd /etc/irodsfsd/config.yaml
	chmod 660 /etc/irodsfsd/config.yaml
	mkdir -p $$(awk '/data_root_path:/ {print $$2}' /etc/irodsfsd/config.yaml)
	chown irodsfsd:irodsfsd $$(awk '/data_root_path:/ {print $$2}' /etc/irodsfsd/config.yaml)
	systemctl daemon-reload

.PHONY: uninstall
uninstall:
	rm -f /usr/bin/irodsfsd
	rm -f /etc/systemd/system/multi-user.target.wants/irodsfsd.service || true
	rm -f /usr/lib/systemd/system/irodsfsd.service
	systemctl daemon-reload
	userdel irodsfsd || true
	groupdel irodsfsd || true
	rm -rf $$(awk '/data_root_path:/ {print $$2}' /etc/irodsfsd/config.yaml)
	rm -rf /etc/irodsfsd
