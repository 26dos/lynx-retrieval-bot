# Overview: Efficient Retrieval with Side-Node Caching Network

## Goal
When a **Piece** is downloaded for the first time and a **Filecoin-compatible full Merkle tree (Fr32 + Poseidon, arity=8)** is built, subsequent retrievals no longer require reading the full data. Instead, clients only need:
- To download the target **sub-range** (e.g., a small window or byte interval), and  
- To fetch a small number of **siblings** from the **Side-Net** (O(logN) × 32B) from the window root to the Piece root,  

to recompute and verify the **PieceCID (CommP)** locally. This yields **high efficiency, low latency, and low bandwidth cost**.

The system consists of two main components:
- **retrieval-bot**: responsible for discovering/generating retrieval tasks, resolving providers, downloading data, and recording results.
- **Side-Net**: responsible for building and caching full Merkle trees (or window paths) and providing a read-only side-node service.

---

## 1. retrieval-bot: Retrieval Pipeline

retrieval-bot is a modular pipeline with the following submodules:

### 1.1 Ingestion & Scheduler
- **Purpose**: Collects retrievable **Pieces** or **Payloads** from chain/index/internal sources and generates retrieval tasks.
- **Details**:
  - Tasks are enqueued into a task queue.  
  - Supports **grouping & sampling** (e.g., group by client+provider, keep top 30%).  
  - Supports **batching & throttling** to prevent overload.  

### 1.2 Resolver
- **Provider Resolver**: Maps provider/miner IDs to **libp2p multiaddrs**, geolocation, and connectivity info.  
- **Location Resolver**: Maps multiaddrs to public IP/port and augments with geolocation.  
- **Usage**: Provides concrete addresses for workers to attempt retrieval.  

### 1.3 Process Manager
- **Purpose**: Spawns multiple worker processes (e.g., `http_worker`, `graphsync_worker`, `bitswap_worker`) with configured concurrency, restarts crashed workers, and attaches log labels.
- **Value**: Centralizes orchestration and reliability.

### 1.4 Task Workers
- **Purpose**: Poll the task queue, execute tasks via protocol workers, and record results.  
- **Protocol Workers**:
  - **HTTP Worker**: Downloads via `http://ip:port/piece/<piece_cid>`.  
  - **Graphsync Worker**: Retrieves via Graphsync protocol.  
  - **Bitswap Worker**: Downloads via Bitswap for hot data.  
- **Timeouts & Retries**: Per connection, first byte, and overall duration. Supports backoff and retries.

### 1.5 Result Emitter
- **Purpose**: Emits an event when a Piece is **fully downloaded and verified**. This event signals **Side-Net** to begin tree construction.
- **Key Requirement**: Must be **idempotent** and **deduplicated**.

---

## 2. Side-Net: Side-Node Construction and Caching

When retrieval-bot signals success for a Piece, Side-Net takes over:

### 2.1 Fetcher
- **Purpose**: Downloads the full Piece/object set from the resolved provider address.  
- **Rule**: If the input is incomplete or corrupted, abort construction and mark the source as failed.  

### 2.2 Builder
- **Goal**: Build a **Filecoin-compatible Merkle tree**:
  - **Leaves**: Apply **Fr32 padding** (bit-level mapping to BLS12-381 field elements).  
  - **Hash**: Use **Poseidon** with **8-arity branching**.  
  - **Root**: The root is the **PieceCID (CommP)**.  
  - **Validation**: Compare the computed root with the on-chain PieceCID; discard if mismatched.  

- **Window and Paths**:
  - Split data into fixed **windows** (e.g., 1MiB).  
  - Compute **window roots** and export **window-paths** (siblings from window root to Piece root).  
  - Optionally export `(level, index) → node_hash` for more granular storage.  

### 2.3 Cache-Store
- **Purpose**: Persist **window-paths** (or side-nodes) in a KV/DB/object store and expose read-only APIs:  
  - `GET /sidenodes/{piece}/{level}/{index}` → 32B node.  
  - `GET /window-path/{piece}/{window_id}` → list of siblings.  

- **Replication**: Hot Pieces’ side-nodes can be replicated to POP/CDN/Redis for faster access.  
- **Trust Model**: Cache is untrusted; incorrect siblings will fail root validation.  

### 2.4 Lifecycle
- **Eviction**: Based on hotness, time, or capacity.  
- **Recompute/Repair**: Triggered if roots mismatch or corruption suspected.  
- **Idempotent**: Multiple workers can attempt the same Piece build without conflict.  

---

## 3. End-to-End Retrieval Flow

### First Retrieval (no cache)
1. retrieval-bot downloads a Piece fully.  
2. Side-Net constructs a full Merkle tree (Fr32 + Poseidon).  
3. Validates root, stores window-paths in cache.  
4. Future retrievals can use the cache.  

### Subsequent Retrievals (with cache)
1. retrieval-bot maps payload CID → `(piece, offset, length, window_id)`.  
2. Downloads only the required sub-range within the window.  
3. Locally merges leaves → window root.  
4. Fetches `window-path(piece, window_id)` from Side-Net.  
5. Recomputes root and compares with on-chain CommP.  

**Bandwidth cost**: `sub-range size + O(logN) × 32B`.  

---

## 4. Failure & Fallback

- **Incomplete downloads**: abort build, mark failure.  
- **Incorrect cache nodes**: root mismatch → proof fails immediately.  
- **Unreachable provider**: log failure, retry with backoff.  
- **Unavailable Side-Net**: fallback to larger-range download.  

---

## 5. Scalability & Consistency

- **Scalability**:  
  - retrieval-bot can shard tasks; workers scale horizontally.  
  - Side-Net cache can be replicated across POPs and scaled independently.  

- **Consistency**:  
  - Filecoin-compatible hashing: Fr32 + Poseidon (arity=8).  
  - Fixed window size, leaf granularity = 32B.  
  - Include metadata (version, parameters) for reproducibility.  
  - Trustless verification: only mathematical root comparison matters.  

---

## 6. Observability & Deployment

- **Metrics**: success rate, build QPS, cache hit rate, fallback count.  
- **Logging**: correlation IDs linking download → build → cache → proof.  
- **Prewarming**: offline pre-build for hot Pieces.  
- **Practical Interfaces**: `window_id → siblings[]` covers most use cases.  

---

## 7. Key Takeaways

1. **First build quality matters most**: Golden source for all future retrievals.  
2. **retrieval-bot handles protocol diversity** (HTTP, Graphsync, Bitswap).  
3. **Window-path API is the sweet spot**: practical and bandwidth efficient.  
4. **Strict root validation** ensures correctness without trusting caches.  
5. **Idempotency** avoids conflicts and redundant work.  
6. **Version pinning**: Poseidon params, Fr32 implementation, window size must be fixed and recorded.  
