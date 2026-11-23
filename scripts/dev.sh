#!/bin/sh
set -eu

case ${1:-} in
    up|down|status|redis-cluster-up) action=$1 ;;
    *) printf 'Usage: sh %s {up|down|status|redis-cluster-up}\n' "$0" >&2; exit 2 ;;
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
redis_addresses=slg-dev-redis-1:6379,slg-dev-redis-2:6379,slg-dev-redis-3:6379
if [ "$network" = gm-dev ]; then
    redis_addresses=gm-redis-1:6379,gm-redis-2:6379,gm-redis-3:6379
fi
http_port=${SLG_GATEWAY_HTTP_PORT:-8080}
tcp_port=${SLG_GATEWAY_TCP_PORT:-19090}
image=localhost/slg-dev:local
gateway=slg-dev-gateway
login_server=slg-dev-login-server
game_server=slg-dev-game-server
owner_label=com.numberman1.slg.environment=${connection:-local}

run_podman() {
    if [ -n "$connection" ]; then
        set -- --connection "$connection" "$@"
    fi
    # 禁止 Git Bash 转换参数，确保 etcd 键前缀和凭据按原值传给 podman.exe。
    MSYS2_ARG_CONV_EXCL='*' MSYS2_ENV_CONV_EXCL='SLG_;REDISCLI_AUTH' podman "$@"
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
                # 同时清理由旧版启动脚本创建且带本环境标签的容器。
                "$gateway"|"$login_server"|"$game_server"|slg-dev-battle-server) run_podman rm --force "$name" ;;
            esac
        done
        exit 0
        ;;
esac

if [ "$action" = redis-cluster-up ] && [ "$network" = gm-dev ]; then
    printf 'The gm-dev Compose project manages Redis. Run services.ps1 redis-up in dev-services.\n' >&2
    exit 1
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

require_owned_container() {
    owner=$(run_podman container inspect --format '{{index .Config.Labels "com.numberman1.slg.environment"}}' "$1")
    if [ "com.numberman1.slg.environment=$owner" != "$owner_label" ]; then
        printf 'Container %s already exists outside this SLG setup.\n' "$1" >&2
        exit 1
    fi
}

if [ "$action" = redis-cluster-up ]; then
    : "${SLG_REDIS_PASSWORD:?Set SLG_REDIS_PASSWORD for the local Redis Cluster}"
    export SLG_REDIS_PASSWORD
    export REDISCLI_AUTH=$SLG_REDIS_PASSWORD
    redis_cli() {
        node=$1
        shift
        run_podman exec --env REDISCLI_AUTH "$node" redis-cli "$@"
    }
    for name in slg-dev-redis-1 slg-dev-redis-2 slg-dev-redis-3; do
        if run_podman container exists "$name"; then
            require_owned_container "$name"
            run_podman start "$name" >/dev/null
        else
            run_podman run --detach --name "$name" --network "$network" --hostname "$name" \
                --label "$owner_label" --env SLG_REDIS_PASSWORD --volume "$name-data:/data" \
                docker.io/library/redis:8.2.10-alpine sh -ec '
                    exec redis-server --cluster-enabled yes --cluster-config-file nodes.conf \
                        --cluster-announce-hostname "$HOSTNAME" --cluster-preferred-endpoint-type hostname \
                        --requirepass "$SLG_REDIS_PASSWORD" --masterauth "$SLG_REDIS_PASSWORD"
                ' >/dev/null
        fi
    done
    new_nodes=0
    for name in slg-dev-redis-1 slg-dev-redis-2 slg-dev-redis-3; do
        attempt=0
        until info=$(redis_cli "$name" cluster info 2>/dev/null) && printf '%s\n' "$info" | grep -q '^cluster_state:'; do
            attempt=$((attempt + 1))
            if [ "$attempt" -ge 20 ]; then
                printf 'Cannot read cluster info from %s. Check the password and container logs.\n' "$name" >&2
                exit 1
            fi
            sleep 1
        done
        if printf '%s\n' "$info" | grep -q '^cluster_known_nodes:1[[:space:]]*$' &&
            printf '%s\n' "$info" | grep -q '^cluster_slots_assigned:0[[:space:]]*$'; then
            new_nodes=$((new_nodes + 1))
        fi
    done
    if [ "$new_nodes" -eq 3 ]; then
        # 只有全部节点尚未加入集群时才分配槽位，已有节点继续使用持久化配置。
        redis_cli slg-dev-redis-1 --cluster create slg-dev-redis-1:6379 slg-dev-redis-2:6379 slg-dev-redis-3:6379 --cluster-yes
    fi
    for name in slg-dev-redis-1 slg-dev-redis-2 slg-dev-redis-3; do
        attempt=0
        until redis_cli "$name" cluster info | grep -q '^cluster_state:ok[[:space:]]*$'; do
            attempt=$((attempt + 1))
            if [ "$attempt" -ge 20 ]; then
                printf 'Redis Cluster is not ready on %s. Inspect cluster info and node logs.\n' "$name" >&2
                exit 1
            fi
            sleep 1
        done
    done
    printf 'Redis Cluster ready on %s: slg-dev-redis-{1,2,3}:6379\n' "$network"
    exit 0
fi

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
: "${SLG_MYSQL_DSN:?Set SLG_MYSQL_DSN to a MySQL database reachable from the SLG containers}"
export SLG_MYSQL_DSN
export SLG_REDIS_MODE=${SLG_REDIS_MODE:-cluster}
if [ "$SLG_REDIS_MODE" = cluster ]; then
    export SLG_REDIS_ADDRESSES=${SLG_REDIS_ADDRESSES:-$redis_addresses}
else
    : "${SLG_REDIS_ADDRESSES:?Set SLG_REDIS_ADDRESSES for standalone Redis}"
    export SLG_REDIS_ADDRESSES
fi
export SLG_REDIS_USERNAME=${SLG_REDIS_USERNAME:-}
export SLG_REDIS_PASSWORD=${SLG_REDIS_PASSWORD:-}
export SLG_REDIS_KEY_PREFIX=${SLG_REDIS_KEY_PREFIX:-slg:login}
export SLG_ETCD_ENDPOINTS=${SLG_ETCD_ENDPOINTS:-http://gm-etcd-1:2379,http://gm-etcd-2:2379,http://gm-etcd-3:2379}
export SLG_ETCD_PREFIX=${SLG_ETCD_PREFIX:-/slg/services}
export SLG_ETCD_USERNAME=${SLG_ETCD_USERNAME:-}
export SLG_ETCD_PASSWORD=${SLG_ETCD_PASSWORD:-}

for name in "$gateway" "$login_server" "$game_server"; do
    if run_podman container exists "$name"; then
        require_owned_container "$name"
    fi
done

target_arch=$(run_podman info --format '{{.Host.Arch}}')
build_dir="$server_dir/.build/podman"
mkdir -p "$build_dir"
(
    cd "$server_dir"
    export GOOS=linux GOARCH="$target_arch" CGO_ENABLED=0
    for service in gateway login-server; do
        go build -mod=readonly -trimpath -o "$build_dir/$service" "./cmd/$service"
    done
)

# 游戏服由容器内工具链构建，和 Go 服务使用同一目标架构。
game_source="$server_dir/internal/game-server"
game_context="$build_dir/internal/game-server"
battle_source="$server_dir/internal/battle"
battle_context="$build_dir/internal/battle"
cpp_source="$server_dir/internal/cpp"
cpp_context="$build_dir/internal/cpp"
deps_dir="$server_dir/.build/game-deps"
orbit_stage="$game_context/orbit-cpp"
if command -v cygpath >/dev/null 2>&1; then
    deps_dir=$(cygpath -m "$deps_dir")
    orbit_stage=$(cygpath -m "$orbit_stage")
fi
cmake "-DSLG_GAME_DEPS_DIR=$deps_dir" -P "$game_source/dependencies.cmake"
cmake "-DSLG_ORBIT_STAGE_DIR=$orbit_stage" -P "$cpp_source/orbit.cmake"
mkdir -p "$game_context/deps" "$game_context/src" "$battle_context/src" "$battle_context/include" "$cpp_context"
cp "$deps_dir/json.hpp" "$deps_dir/httplib.h" "$game_context/deps/"
cp "$game_source/CMakeLists.txt" "$game_source/dependencies.cmake" "$game_context/"
cp "$game_source/src/"* "$game_context/src/"
cp "$battle_source/CMakeLists.txt" "$battle_context/"
cp "$battle_source/src/"* "$battle_context/src/"
cp -R "$battle_source/include/slg" "$battle_context/include/"
cp "$cpp_source/orbit.cmake" "$cpp_source/address.hpp" "$cpp_context/"

containerfile="$server_dir/deploy/Containerfile"
if command -v cygpath >/dev/null 2>&1; then
    containerfile=$(cygpath -m "$containerfile")
    build_dir=$(cygpath -m "$build_dir")
fi
run_podman build --file "$containerfile" --tag "$image" "$build_dir"

# 先准备账号表及可选演示账号，再启动常驻服务。
run_podman run --rm --network "$network" --env SLG_MYSQL_DSN "$image" /slg/login-server migrate
if [ -n "${SLG_DEMO_PASSWORD:-}" ]; then
    export SLG_ACCOUNT=${SLG_DEMO_ACCOUNT:-demo}
    export SLG_ACCOUNT_PASSWORD=$SLG_DEMO_PASSWORD
    run_podman run --rm --network "$network" --env SLG_MYSQL_DSN \
        --env SLG_ACCOUNT --env SLG_ACCOUNT_PASSWORD "$image" /slg/login-server create-account
fi

# 应用容器通过 Podman 网络 DNS 访问依赖和其他服务。
set -- run --detach --replace --http-proxy=false --network "$network" --label "$owner_label"
run_podman "$@" --name "$game_server" --env SLG_GAME_ADDR=0.0.0.0:8081 --env SLG_GAME_RPC_ADDR=0.0.0.0:8083 \
    --env SLG_WORLD_IDLE_SECONDS \
    "$image" /slg/game-server
run_podman "$@" --name "$login_server" --env SLG_LOGIN_ADDR=0.0.0.0:8082 \
    --env "SLG_LOGIN_ADVERTISE_URL=http://$login_server:8082" \
    --env SLG_LOGIN_CONCURRENCY --env SLG_REGISTER_CONCURRENCY --env SLG_REGISTER_RATE_PER_SECOND \
    --env SLG_MYSQL_DSN --env SLG_REDIS_MODE --env SLG_REDIS_ADDRESSES \
    --env SLG_REDIS_USERNAME --env SLG_REDIS_PASSWORD --env SLG_REDIS_KEY_PREFIX \
    --env SLG_ETCD_ENDPOINTS --env "SLG_ETCD_PREFIX=$SLG_ETCD_PREFIX" --env SLG_ETCD_USERNAME --env SLG_ETCD_PASSWORD \
    "$image" /slg/login-server
run_podman "$@" --name "$gateway" \
    --publish "127.0.0.1:$http_port:8080" --publish "127.0.0.1:$tcp_port:$tcp_port" \
    --env SLG_GATEWAY_ADDR=0.0.0.0:8080 --env "SLG_GATEWAY_TCP_ADDR=0.0.0.0:$tcp_port" \
    --env "SLG_GAME_UPSTREAM_URL=http://$game_server:8081" \
    --env "SLG_GAME_RPC_UPSTREAM=$game_server:8083" \
    --env SLG_REDIS_MODE --env SLG_REDIS_ADDRESSES \
    --env SLG_REDIS_USERNAME --env SLG_REDIS_PASSWORD --env SLG_REDIS_KEY_PREFIX \
    --env SLG_ETCD_ENDPOINTS --env "SLG_ETCD_PREFIX=$SLG_ETCD_PREFIX" --env SLG_ETCD_USERNAME --env SLG_ETCD_PASSWORD \
    "$image" /slg/gateway

# 通过实际网关验证登录依赖、Redis 票据读写及 Orbit 玩法连接。
url="http://127.0.0.1:$http_port"
attempt=0
while [ "$attempt" -lt 20 ]; do
    if curl --noproxy '*' --fail --silent --max-time 3 "$url/readyz" >/dev/null; then
        printf 'SLG is ready (HTTP 127.0.0.1:%s, TCP 127.0.0.1:%s).\n' "$http_port" "$tcp_port"
        exit 0
    fi
    attempt=$((attempt + 1))
    sleep 1
done
printf 'SLG did not become ready. Inspect the %s, %s and %s container logs.\n' "$gateway" "$login_server" "$game_server" >&2
exit 1
