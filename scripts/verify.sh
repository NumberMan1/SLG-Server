#!/bin/sh
set -eu

server_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
game_dir=${SLG_GAME_BUILD_DIR:-"$server_dir/.build/internal/game"}
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
executable_path() (
    root=$1
    name=$2
    for dir in "$root" "$root/Debug"; do
        for executable in "$dir/$name" "$dir/$name.exe"; do
            if [ -f "$executable" ]; then
                if command -v cygpath >/dev/null 2>&1; then
                    cygpath -m "$executable"
                else
                    printf '%s\n' "$executable"
                fi
                exit 0
            fi
        done
    done
    printf 'C++ %s executable was not found.\n' "$name" >&2
    exit 1
)

build "$server_dir/internal/game-server" "$game_dir" "-DSLG_GAME_DEPS_DIR=$deps_dir" -DBUILD_TESTING=ON
SLG_TEST_GAME_SERVER=$(executable_path "$game_dir" game-server)
export SLG_TEST_GAME_SERVER

(
    cd "$server_dir"
    go test ./...
    go vet ./...
)

ctest --test-dir "$game_dir" -C Debug --output-on-failure
