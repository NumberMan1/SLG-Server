#!/bin/sh
set -eu

server_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
client_dir=$(CDPATH='' cd -- "${SLG_CLIENT_DIR:-"$server_dir/../client"}" && pwd)
protoc=$(command -v "${PROTOC:-protoc}")
go_plugin=$(command -v "${PROTOC_GEN_GO:-protoc-gen-go}")
output="$client_dir/Assets/Scripts/Protocol"

mkdir -p "$output"
if command -v cygpath >/dev/null 2>&1; then
    go_plugin=$(cygpath -m "$go_plugin")
    output=$(cygpath -m "$output")
fi

cd "$server_dir"
"$protoc" --proto_path=. "--plugin=protoc-gen-go=$go_plugin" --go_out=. \
    --go_opt=paths=source_relative "--csharp_out=$output" proto/session.proto
