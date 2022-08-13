# /bin/bash
go build -ldflags "-s -w" frontend.go; zip frontend.zip frontend
go build-ldflags "-s -w" backend.go; zip backend.zip backend
