#!/bin/sh
set -e

# Start Docker daemon in the background using the dind entrypoint
/usr/bin/start-dockerd.sh &

# Wait for Docker daemon to be ready
echo "Waiting for Docker daemon to start..."
for i in $(seq 1 30); do
    if docker info >/dev/null 2>&1; then
        echo "Docker daemon is ready."
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "Error: Docker daemon did not start within 30 seconds, aborting."
        exit 1
    fi
    sleep 1
done

# Start the sandbox executor
exec /usr/bin/sandbox-executor-bin "$@"
