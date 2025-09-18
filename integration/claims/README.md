# Filecoin Claims Importer

This project is a **Go service** that periodically imports Filecoin **verified registry claims** from daily dump files into MongoDB.  
It ensures that only **active storage providers (with power)** are included and performs efficient upserts with batching.

---

## 📌 About `all_claims_%s.json`

The `all_claims_YYYYMMDD.json` file is a **daily export of all verified registry claims** on the Filecoin network.  
It usually comes from **Lotus state dump jobs** (e.g., via `StateGetAllClaims` RPC or custom snapshot exporters) that generate JSON files of the current claim set.

- File format: JSON-RPC style object
- Keys: claim IDs
- Values: claim details (`Provider`, `Client`, `Data`, `Size`, `TermMin`, `TermMax`, `TermStart`, `Sector`)

Example path:
```
all_claims_20250115.json
```

The service expects these files to be present in the configured `CLAIMS_DUMP_DIR`.  
After processing, the file will be **deleted** to avoid re-ingestion.

---

## ⚙️ How It Works

### 1. Configuration
Environment variables configure Lotus, MongoDB, and processing behavior:

| Variable | Description | Default |
|----------|-------------|---------|
| `FULLNODE_API_URL` | Lotus RPC URL | *required* |
| `FULLNODE_API_TOKEN` | Lotus JWT Token | "" |
| `MONGO_URI` | MongoDB connection string | *required* |
| `MONGO_DB` | Database name | `filstats` |
| `MONGO_CLAIMS_COLL` | Collection name | `claims` |
| `CLAIMS_DUMP_DIR` | Directory containing `all_claims_YYYYMMDD.json` | "." |
| `CLAIMS_BULK_SIZE` | Bulk insert batch size | 2000 |
| `RUN_EVERY_HOURS` | Interval (hours) for scheduled runs | 1 |

---

### 2. Processing Flow

1. **Check for Dump File**
   - Looks for `all_claims_<date>.json` in `CLAIMS_DUMP_DIR`.
   - Verifies the file size is stable (not still being written).

2. **Load Active Providers**
   - Calls Lotus to list miners and filter those with **non-zero power**.
   - Only keeps claims from active providers.

3. **Parse Claims**
   - Reads JSON dump file.
   - Converts fields to Go `DBClaim` model.

4. **Load Existing Keys**
   - Reads MongoDB to build a set of existing `(provider_id, data_cid, sector, term_start)` keys.

5. **Upsert New Claims**
   - Computes difference between dump file and DB.
   - Performs **bulk upsert** with batching (`CLAIMS_BULK_SIZE`).

6. **Cleanup**
   - Deletes the processed JSON file.
   - Logs stats (`inserted`, `prepared`, `duration`, etc.).

7. **Scheduler**
   - Runs once immediately.
   - Then repeats every `RUN_EVERY_HOURS` (default: 1 hour).

---

### 3. Data Model

MongoDB `claims` collection document example:

```json
{
  "claim_id": 12345,
  "provider_id": 1001,
  "client_id": 5678,
  "client_addr": "f1abcd...",
  "data_cid": "bafy...",
  "size": 34359738368,
  "term_min": 518400,
  "term_max": 1555200,
  "term_start": 123456,
  "sector": 100,
  "miner_addr": "f01001",
  "updated_at": "2025-01-15T12:00:00Z"
}
```

Indexes:
- Unique: `(provider_id, data_cid, sector, term_start)`
- Optional unique: `(provider_id, claim_id)`
- Auxiliary: `client_addr`, `miner_addr`, `updated_at`

---

## 🚀 Running the Service

### Local Run

```bash
export FULLNODE_API_URL=https://api.node.glif.io/rpc/v0
export FULLNODE_API_TOKEN=<your-jwt>
export MONGO_URI=mongodb://127.0.0.1:27017
export MONGO_DB=filstats
export MONGO_CLAIMS_COLL=claims
export CLAIMS_DUMP_DIR=/data/claims
export RUN_EVERY_HOURS=1

go run main.go
```

### Build

```bash
go build -o claims-importer main.go
./claims-importer
```

---

## 🐳 Docker Deployment

### Dockerfile

```dockerfile
FROM golang:1.22 AS builder
WORKDIR /app
COPY . .
RUN go mod tidy && go build -o claims-importer main.go

FROM debian:bookworm-slim
WORKDIR /root/
COPY --from=builder /app/claims-importer .
CMD ["./claims-importer"]
```

### Docker Compose

```yaml
version: '3.8'
services:
  claims-importer:
    build: .
    container_name: claims-importer
    restart: always
    environment:
      - FULLNODE_API_URL=https://api.node.glif.io/rpc/v0
      - FULLNODE_API_TOKEN=
      - MONGO_URI=mongodb://mongo:27017
      - MONGO_DB=filstats
      - MONGO_CLAIMS_COLL=claims
      - CLAIMS_DUMP_DIR=/data/claims
      - CLAIMS_BULK_SIZE=2000
      - RUN_EVERY_HOURS=1
    volumes:
      - ./data:/data/claims
    depends_on:
      - mongo

  mongo:
    image: mongo:6
    restart: always
    ports:
      - "27017:27017"
```

---

## 📊 Logs

The service logs:
- Initialization info
- Active provider count
- Claims loaded & inserted
- Scheduler runs

---

## 🔮 Future Improvements

- Support multiple dump sources (Lotus RPC directly).
- Add Prometheus metrics.
- Parallelize bulk insert pipeline.
