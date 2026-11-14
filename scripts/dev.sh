#!/bin/sh
set -eu

case ${1:-} in
    up|down|status) action=$1 ;;
    *) printf 'Usage: sh %s {up|down|status}\n' "$0" >&2; exit 2 ;;
esac

server_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
env_file=${SLG_ENV_FILE:-"$server_dir/.env"}
case $env_file in
    /*|[A-Za-z]:/*) ;;
    *) env_file="$PWD/$env_file" ;;
esac
if [ -f "$env_file" ]; then
    set -a
    . "$env_file"
    set +a
elif [ -n "${SLG_ENV_FILE:-}" ]; then
    printf 'Environment file not found: %s\n' "$env_file" >&2
    exit 1
fi

connection=${SLG_PODMAN_CONNECTION:-}
machine=${SLG_PODMAN_MACHINE:-}
network=${SLG_PODMAN_NETWORK:-slg-dev}
http_port=${SLG_GATEWAY_HTTP_PORT:-8080}
tcp_port=${SLG_GATEWAY_TCP_PORT:-19090}
image=localhost/slg-dev:local
gateway=slg-dev-gateway
game_server=slg-dev-game-server
owner_label=com.numberman1.slg.environment=${connection:-local}

run_podman() {
    if [ -n "$connection" ]; then
        MSYS2_ARG_CONV_EXCL='*' podman --connection "$connection" "$@"
    else
        MSYS2_ARG_CONV_EXCL='*' podman "$@"
    fi
}

owned_containers() {
    run_podman ps --all --filter "label=$owner_label" --format "$1"
}

case $action in
    status)
        owned_containers 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
        exit 0
        ;;
    down)
        owned=$(owned_containers '{{.Names}}')
        for name in $owned; do
            case $name in
                "$gateway"|"$game_server") run_podman rm --force "$name" ;;
            esac
        done
        exit 0
        ;;
esac

for port in "$http_port" "$tcp_port"; do
    case $port in
        ''|*[!0-9]*) printf 'Invalid gateway port: %s\n' "$port" >&2; exit 2 ;;
    esac
    if ! [ "$port" -ge 1 ] || ! [ "$port" -le 65535 ]; then
        printf 'Invalid gateway port: %s\n' "$port" >&2
        exit 2
    fi
done
if [ "$http_port" -eq "$tcp_port" ]; then
    printf 'HTTP and TCP ports must differ.\n' >&2
    exit 2
fi

if [ -n "$machine" ]; then
    machine_state=$(podman machine inspect "$machine" --format '{{.State}}')
    if [ "$machine_state" != running ]; then
        podman machine start "$machine"
    fi
fi

if [ "$network" = slg-dev ]; then
    run_podman network create --ignore "$network" >/dev/null
else
    run_podman network inspect --format '{{.Name}}' "$network" >/dev/null
fi

for name in "$gateway" "$game_server"; do
    if run_podman container exists "$name"; then
        owner=$(run_podman container inspect --format '{{index .Config.Labels "com.numberman1.slg.environment"}}' "$name")
        if [ "com.numberman1.slg.environment=$owner" != "$owner_label" ]; then
            printf 'Container %s already exists outside this SLG setup.\n' "$name" >&2
            exit 1
        fi
    fi
done

target_arch=$(run_podman info --format '{{.Host.Arch}}')
build_dir="$server_dir/.build/podman"
mkdir -p "$build_dir"
(
    cd "$server_dir"
    export GOOS=linux GOARCH="$target_arch" CGO_ENABLED=0
    go build -mod=readonly -trimpath -o "$build_dir/gateway" ./cmd/gateway
    go build -mod=readonly -trimpath -o "$build_dir/game-server" ./cmd/game-server
)

containerfile="$server_dir/deploy/Containerfile"
if command -v cygpath >/dev/null 2>&1; then
    containerfile=$(cygpath -m "$containerfile")
    build_dir=$(cygpath -m "$build_dir")
fi
run_podman build --file "$containerfile" --tag "$image" "$build_dir"

set -- run --detach --replace --network "$network" --label "$owner_label"
run_podman "$@" --name "$game_server" --env SLG_GAME_ADDR=0.0.0.0:8081 "$image" /slg/game-server
export SLG_DEMO_ACCOUNT=${SLG_DEMO_ACCOUNT:-demo}
export SLG_DEMO_PASSWORD=${SLG_DEMO_PASSWORD:-}
run_podman "$@" --name "$gateway" \
    --publish "127.0.0.1:$http_port:8080" --publish "127.0.0.1:$tcp_port:$tcp_port" \
    --env SLG_GATEWAY_ADDR=0.0.0.0:8080 --env "SLG_GATEWAY_TCP_ADDR=0.0.0.0:$tcp_port" \
    --env "SLG_GAME_URL=http://$game_server:8081" --env SLG_DEMO_ACCOUNT --env SLG_DEMO_PASSWORD \
    "$image" /slg/gateway

url="http://127.0.0.1:$http_port/v1/game/health"
attempt=0
while [ "$attempt" -lt 20 ]; do
    if curl --noproxy '*' --fail --silent --max-time 2 "$url" >/dev/null; then
        printf 'SLG is ready (HTTP 127.0.0.1:%s, TCP 127.0.0.1:%s).\n' "$http_port" "$tcp_port"
        exit 0
    fi
    attempt=$((attempt + 1))
    sleep 1
done
printf 'SLG did not become ready. Inspect the %s and %s container logs.\n' "$gateway" "$game_server" >&2
exit 1
