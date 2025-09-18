# FilPlus Integration Worker

This project is a Go service designed to **process Filecoin market deals**, perform **sampling of claims**, and enqueue them as tasks for further retrieval testing. It integrates MongoDB, Lotus, and auxiliary resolvers.

---

## 📌 Features

1. **Aggregation**
  - Groups market deals by `client_addr + miner_addr`.
  - Keeps only the **top 30%** of deals per group (sorted by `claim_id`).

2. **Sampling**
  - Randomly samples up to **100 deals per group**.
  - Ensures a balanced distribution across clients and providers.

3. **Task Queue**
  - Inserts generated tasks into `claims_task_queue`.
  - Respects a maximum `batchSize` to avoid overloading.

4. **Result Storage**
  - Saves task results into `claims_task_result`.

5. **Metrics**
  - Logs tasks per country, continent, and retrieval module.

---

## 🏗 Project Structure

- `main.go` → Entry point, orchestrates grouping, sampling, and task enqueue.
- `getDealsGroupedByClientProvider()` → Groups and trims deals.
- `RunOnce()` → Runs one full cycle of enqueueing tasks & saving results.
- Mongo collections:
  - `claims_task_queue`
  - `claims_task_result`
  - `claims` (market deals source)

---

## ⚙️ Requirements

- Go 1.20+
- MongoDB (with collections above)
- Lotus RPC API access
- Redis (if caching or provider resolving is extended)

---

## 🔑 Environment Variables

| Variable | Description | Example |
|----------|-------------|---------|
| `QUEUE_MONGO_URI` | MongoDB URI for task queue | `mongodb://127.0.0.1:27017` |
| `QUEUE_MONGO_DATABASE` | DB name for queue | `filqueue` |
| `STATEMARKETDEALS_MONGO_URI` | MongoDB URI for market deals | `mongodb://127.0.0.1:27017` |
| `STATEMARKETDEALS_MONGO_DATABASE` | DB name for market deals | `filmarket` |
| `RESULT_MONGO_URI` | MongoDB URI for results | `mongodb://127.0.0.1:27017` |
| `RESULT_MONGO_DATABASE` | DB name for results | `filresult` |
| `LOTUS_API_URL` | Lotus RPC endpoint | `https://api.node.glif.io/rpc/v0` |
| `LOTUS_API_TOKEN` | Lotus API token | `<your-jwt>` |
| `IPINFO_TOKEN` | IPInfo API token | `<your-token>` |

---

## 🚀 Run

```bash
go run main.go