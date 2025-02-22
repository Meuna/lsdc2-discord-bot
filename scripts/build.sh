# /bin/bash
#!/bin/bash

script_dir=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
src_dir=$script_dir/..

mkdir -p $HOME/go/pkg

podman run \
    --rm \
    -v $(pwd):/go/src \
    -v $HOME/go/pkg:/go/pkg \
    --workdir /go/src \
    docker.io/golang:1.24-bullseye \
    /bin/bash -c ' \
        go get ./... && \
        go build -ldflags "-s -w" frontend.go && \
        go build -ldflags "-s -w" backend.go'

zip --junk-paths frontend.zip frontend
zip --junk-paths backend.zip backend
