#!/bin/sh
set -eu

server_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
build_dir=${SLG_BUILD_DIR:-"$server_dir/.build/battle"}
generator=${SLG_CMAKE_GENERATOR:-}

if [ -z "$generator" ] && [ ! -f "$build_dir/CMakeCache.txt" ] && command -v ninja >/dev/null 2>&1; then
    generator=Ninja
fi

(
    cd "$server_dir"
    go test ./...
    go vet ./...
)

set -- -S "$server_dir/battle-server" -B "$build_dir" -DBUILD_TESTING=ON
if [ -n "$generator" ]; then
    set -- "$@" -G "$generator"
fi
cmake "$@"
cmake --build "$build_dir" --config Debug
ctest --test-dir "$build_dir" -C Debug --output-on-failure
