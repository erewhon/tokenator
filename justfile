set shell := ["bash", "-c"]

# default: list recipes
default:
    @just --list

# build the binary into ./bin/
build:
    go build -o bin/tokenator ./cmd/tokenator

# run tests
test:
    go test ./...

# build + install to ~/.local/bin (where the systemd user services expect it)
install: build test
    install -m 755 bin/tokenator ~/.local/bin/tokenator

# run the web UI against the default database
serve: build
    ./bin/tokenator serve
