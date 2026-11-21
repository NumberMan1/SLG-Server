#!/bin/sh
set -eu

server_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
build_dir=${SLG_BUILD_DIR:-"$server_dir/.build/internal/battle"}
game_dir="$server_dir/.build/internal/game"
deps_dir="$server_dir/.build/game-deps"
generator=${SLG_CMAKE_GENERATOR:-}

build() (
    source=$1
    output=$2
    if [ -z "$generator" ] && [ ! -f "$output/CMakeCache.txt" ] && command -v ninja >/dev/null 2>&1; then
        generator=Ninja
    fi
    shift 2
    set -- -S "$source" -B "$output" -DCMAKE_BUILD_TYPE=Debug "$@"
    if [ -n "$generator" ]; then
        set -- "$@" -G "$generator"
    fi
    cmake "$@"
    cmake --build "$output" --config Debug
)

if command -v cygpath >/dev/null 2>&1; then
    deps_dir=$(cygpath -m "$deps_dir")
fi
build "$server_dir/internal/game-server" "$game_dir" "-DSLG_GAME_DEPS_DIR=$deps_dir"
for dir in "$game_dir" "$game_dir/Debug"; do
    for executable in "$dir/game-server" "$dir/game-server.exe"; do
        if [ -f "$executable" ]; then
            SLG_TEST_GAME_SERVER=$executable
            break 2
        fi
    done
done
: "${SLG_TEST_GAME_SERVER:?C++ game-server executable was not found}"
if command -v cygpath >/dev/null 2>&1; then
    SLG_TEST_GAME_SERVER=$(cygpath -m "$SLG_TEST_GAME_SERVER")
fi
export SLG_TEST_GAME_SERVER

(
    cd "$server_dir"
    go test ./...
    go vet ./...
)

build "$server_dir/internal/battle-server" "$build_dir" -DBUILD_TESTING=ON
ctest --test-dir "$build_dir" -C Debug --output-on-failure
