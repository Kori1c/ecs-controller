#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
cd "$SCRIPT_DIR"

# Pass the checked-out revision into the image so the UI can compare it with
# GitHub. The compose project name also lets the updater address this stack
# when the repository lives outside the default installation directory.
compose_project="$(basename "$SCRIPT_DIR")"
export ECS_COMPOSE_PROJECT_NAME="${ECS_COMPOSE_PROJECT_NAME:-$compose_project}"
if git -C "$SCRIPT_DIR" rev-parse --verify HEAD >/dev/null 2>&1; then
    export ECS_COMMIT="$(git -C "$SCRIPT_DIR" rev-parse HEAD)"
    export ECS_VERSION="$(git -C "$SCRIPT_DIR" rev-parse --short=8 HEAD)"
    export ECS_BUILD_DATE="$(git -C "$SCRIPT_DIR" show -s --format=%cI HEAD)"
    export ECS_IMAGE_TAG="${ECS_IMAGE_TAG:-sha-$ECS_COMMIT}"
else
    export ECS_IMAGE_TAG="${ECS_IMAGE_TAG:-latest}"
fi
export ECS_IMAGE_REPOSITORY="${ECS_IMAGE_REPOSITORY:-docker.io/kori1c/ecs-controller}"
export ECS_UPDATER_IMAGE_REPOSITORY="${ECS_UPDATER_IMAGE_REPOSITORY:-docker.io/kori1c/ecs-controller-updater}"
export ECS_UPDATER_IMAGE_TAG="${ECS_UPDATER_IMAGE_TAG:-latest}"

usage() {
    cat <<'EOF'
Usage: ./deploy.sh [--no-build]

Pull prebuilt images and start ecs-controller with Docker Compose, then wait for /healthz.

Environment:
  ECS_SETUP_TOKEN  One-time token used during first-run initialization.
EOF
}

no_build=0
case "${1:-}" in
    "") ;;
    --no-build) no_build=1 ;; # Kept for compatibility; deployment never builds locally.
    --help|-h) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
esac

if ! command -v docker >/dev/null 2>&1; then
    echo "Docker is not installed or not available in PATH." >&2
    exit 1
fi

compose_cmd=()
if docker compose version >/dev/null 2>&1; then
    compose_cmd=(docker compose)
elif command -v docker-compose >/dev/null 2>&1 && docker-compose version >/dev/null 2>&1; then
    compose_cmd=(docker-compose)
else
    echo "Docker Compose is not available. Install the Docker Compose plugin." >&2
    exit 1
fi

if ! docker info >/dev/null 2>&1; then
    echo "Docker is not running. Start Colima with: colima start" >&2
    exit 1
fi

generated_token=0
# Preserve the current token when redeploying this same Compose project.
# Do not copy credentials from an unrelated container with a matching name.
if [[ -z "${ECS_SETUP_TOKEN:-}" ]]; then
    existing_project="$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' ecs-controller 2>/dev/null || true)"
    if [[ "$existing_project" == "$ECS_COMPOSE_PROJECT_NAME" ]]; then
        existing_environment="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' ecs-controller 2>/dev/null || true)"
        while IFS= read -r entry; do
            case "$entry" in ECS_SETUP_TOKEN=*) ECS_SETUP_TOKEN="${entry#ECS_SETUP_TOKEN=}"; export ECS_SETUP_TOKEN; break ;; esac
        done <<< "$existing_environment"
        unset existing_environment entry
    fi
fi
if [[ -z "${ECS_SETUP_TOKEN:-}" ]]; then
    if command -v openssl >/dev/null 2>&1; then
        ECS_SETUP_TOKEN="$(openssl rand -hex 16)"
    else
        ECS_SETUP_TOKEN="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
    fi
    export ECS_SETUP_TOKEN
    generated_token=1
fi

echo "Pulling prebuilt ecs-controller images..."
"${compose_cmd[@]}" pull
echo "Starting ecs-controller..."
"${compose_cmd[@]}" up -d --no-build --remove-orphans

container_name="$("${compose_cmd[@]}" ps -q ecs-controller)"
if [[ -z "$container_name" ]]; then
    echo "The ecs-controller container was not created." >&2
    exit 1
fi

updater_container="$("${compose_cmd[@]}" ps -q ecs-controller-updater)"
if [[ -z "$updater_container" ]]; then
    echo "The ecs-controller updater container was not created." >&2
    "${compose_cmd[@]}" logs --tail=80 ecs-controller-updater >&2 || true
    exit 1
fi

health_state="starting"
for _ in {1..30}; do
    health_state="$(docker inspect --format '{{.State.Health.Status}}' "$container_name" 2>/dev/null || true)"
    if [[ "$health_state" == "healthy" ]]; then
        break
    fi
    if [[ "$health_state" == "unhealthy" || "$health_state" == "" ]]; then
        echo "Container health check failed: ${health_state:-unknown}" >&2
        "${compose_cmd[@]}" logs --tail=80 ecs-controller >&2 || true
        exit 1
    fi
    sleep 2
done

if [[ "$health_state" != "healthy" ]]; then
    echo "Timed out waiting for ecs-controller health check: $health_state" >&2
    "${compose_cmd[@]}" logs --tail=80 ecs-controller >&2 || true
    exit 1
fi

updater_state="$(docker inspect --format '{{.State.Status}}' "$updater_container" 2>/dev/null || true)"
if [[ "$updater_state" != "running" ]]; then
    echo "The ecs-controller updater is not running: ${updater_state:-unknown}" >&2
    "${compose_cmd[@]}" logs --tail=80 ecs-controller-updater >&2 || true
    exit 1
fi

echo "ecs-controller is healthy."
echo "Open: http://127.0.0.1:43211"
if [[ "$generated_token" -eq 1 ]]; then
    echo "Setup token (only needed before initialization): $ECS_SETUP_TOKEN"
    echo "Save this token if initialization is not complete yet."
else
    echo "Using the supplied or existing initialization token."
    echo "Read it on this server with: docker exec ecs-controller printenv ECS_SETUP_TOKEN"
fi
echo "Already initialized? Log in with the administrator password; the setup token cannot reset it."
