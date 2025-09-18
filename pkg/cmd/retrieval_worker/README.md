# Process Manager

This project provides a **Process Manager** that supervises and runs worker modules (`graphsync_worker`, `http_worker`, `bitswap_worker`, etc.) with configurable concurrency.

It handles:
- Automatic spawning of worker processes based on configuration.
- Graceful shutdown when the context is canceled.
- Automatic restart with a delay if a worker process crashes.
- Injects public IP information into the environment for workers to use.

---

## 🔧 Features

- Run multiple worker modules concurrently.
- Restart crashed processes after a configurable error interval.
- Injects runtime environment variables like IP, ISP, location, etc.
- Uses **MongoDB** to connect to `queue` and `result` databases for task management.
- Integrated structured logging (`go-log/v2`) with correlation IDs.

---

## 🚀 Run Requirements

Before starting the program, you **must export the following environment variables**:

```bash
# Worker modules to run (comma-separated paths)
export PROCESS_MODULES=./graphsync_worker,./http_worker,./bitswap_worker

# Restart interval if a worker crashes
export PROCESS_ERROR_INTERVAL=5s

# Task worker configuration
export TASK_WORKER_POLL_INTERVAL=30s
export TASK_WORKER_TIMEOUT_BUFFER=10s

# MongoDB connections
export QUEUE_MONGO_URI="mongodb+srv://user:pass@host/?retryWrites=true&w=majority"
export QUEUE_MONGO_DATABASE=test
export RESULT_MONGO_URI="mongodb+srv://user:pass@host/?retryWrites=true&w=majority"
export RESULT_MONGO_DATABASE=test

# Optional filters
export ACCEPTED_CONTINENTS=
export ACCEPTED_COUNTRIES=

# Worker concurrency
export CONCURRENCY_GRAPHSYNC_WORKER=10
export CONCURRENCY_BITSWAP_WORKER=10
export CONCURRENCY_HTTP_WORKER=10

# Logging configuration
export GOLOG_LOG_LEVEL="panic,convert=info,env=debug,bitswap_client=info,graphsync_client=info,http_client=info,process-manager=info,task-worker=info,bitswap_worker=info"
export GOLOG_LOG_FMT=json
```

---

## ⚙️ How It Works

1. **Initialization**
   - Reads `PROCESS_MODULES` and checks executables exist in `$PATH`.
   - Determines concurrency for each module from `CONCURRENCY_*` env vars.
   - Fetches public IP info (`IP`, `City`, `Region`, `Country`, `Continent`, `ASN`, `ISP`) and injects them into environment variables.

2. **Process Spawning**
   - For each module, spawns the configured number of concurrent workers.
   - Each worker runs in its own goroutine and process.
   - Each worker process is tagged with a unique `correlation_id`.

3. **Error Handling**
   - If a worker exits unexpectedly, the manager waits `PROCESS_ERROR_INTERVAL` before restarting it.
   - If the context is canceled (e.g., program termination), all workers are stopped gracefully.

---

## 🛠️ Deployment

### Local Build & Run
```bash
go build -o process-manager ./cmd/process-manager
./process-manager
```

### Docker Deployment
Create a `Dockerfile`:

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o process-manager ./cmd/process-manager

FROM alpine:3.18
WORKDIR /app
COPY --from=builder /app/process-manager .
CMD ["./process-manager"]
```

Build & run:

```bash
docker build -t process-manager .
docker run --rm -it   -e PROCESS_MODULES="./graphsync_worker,./http_worker,./bitswap_worker"   -e PROCESS_ERROR_INTERVAL=5s   -e TASK_WORKER_POLL_INTERVAL=30s   -e TASK_WORKER_TIMEOUT_BUFFER=10s   -e QUEUE_MONGO_URI="mongodb+srv://user:pass@host/?retryWrites=true&w=majority"   -e QUEUE_MONGO_DATABASE=test   -e RESULT_MONGO_URI="mongodb+srv://user:pass@host/?retryWrites=true&w=majority"   -e RESULT_MONGO_DATABASE=test   -e CONCURRENCY_GRAPHSYNC_WORKER=10   -e CONCURRENCY_BITSWAP_WORKER=10   -e CONCURRENCY_HTTP_WORKER=10   -e GOLOG_LOG_LEVEL="panic,convert=info,env=debug"   -e GOLOG_LOG_FMT=json   process-manager
```


