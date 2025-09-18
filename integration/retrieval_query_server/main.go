// main.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Config struct {
	MongoURI  string
	MongoDB   string
	RedisAddr string
	RedisDB   int
	BindAddr  string
}

var (
	cfg       Config
	mgo       *mongo.Client
	db        *mongo.Database
	colResult *mongo.Collection // Mongo collection: claims_task_result
	rds       *redis.Client
)

const (
	redisTTL        = 24 * time.Hour
	statsPeriod     = 24 * time.Hour
	defaultBind     = ":8787"
	zsetMinerHTTP   = "idx:miners:http" // score = HTTP success rate
	keyMinerPrefix  = "stats:miner:"    // stats:miner:<miner_id>
	keyClientPrefix = "stats:client:"   // stats:client:<client_addr> (value = JSON array of items)
	defaultPageSize = 15
	maxPageSize     = 200
)

type RateDoc struct {
	SuccessRateHTTP      float64 `json:"success_rate_http"`
	SuccessRateGraphsync float64 `json:"success_rate_graphsync"`
	SuccessRateBitswap   float64 `json:"success_rate_bitswap"`
}

// Client statistics item (one entry per miner under a client)
type ClientMinerItem struct {
	ClientAddr           string  `json:"client_addr"`
	MinerAddr            string  `json:"miner_addr"`
	SuccessRateHTTP      float64 `json:"success_rate_http"`
	SuccessRateGraphsync float64 `json:"success_rate_graphsync"`
	SuccessRateBitswap   float64 `json:"success_rate_bitswap"`
}

type aggOut2Keys struct {
	ID struct {
		Client string `bson:"client"`
		Miner  string `bson:"miner"`
	} `bson:"_id"`
	Total int64 `bson:"total"`
	OK    int64 `bson:"ok"`
}

type aggOut1Key struct {
	ID    string `bson:"_id"`
	Total int64  `bson:"total"`
	OK    int64  `bson:"ok"`
}

func mustInit() {
	cfg = Config{
		MongoURI:  getenv("MONGO_URI", "mongodb://127.0.0.1:27017"),
		MongoDB:   getenv("MONGO_DB", "fil"),
		RedisAddr: getenv("REDIS_ADDR", "127.0.0.1:6379"),
		RedisDB:   mustAtoi(getenv("REDIS_DB", "0")),
		BindAddr:  getenv("BIND_ADDR", defaultBind),
	}

	var err error
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mgo, err = mongo.Connect(ctx, options.Client().ApplyURI(cfg.MongoURI))
	if err != nil {
		log.Fatalf("mongo connect: %v", err)
	}
	if err := mgo.Ping(ctx, nil); err != nil {
		log.Fatalf("mongo ping: %v", err)
	}
	db = mgo.Database(cfg.MongoDB)
	colResult = db.Collection("claims_task_result")

	rds = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, DB: cfg.RedisDB})
	if err := rds.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("redis ping: %v", err)
	}
	log.Printf("init ok. mongo=%s db=%s redis=%s bind=%s", cfg.MongoURI, cfg.MongoDB, cfg.RedisAddr, cfg.BindAddr)
}

func startCron() {
	go func() {
		runOnce()
		ticker := time.NewTicker(statsPeriod)
		defer ticker.Stop()
		for range ticker.C {
			runOnce()
		}
	}()
}

func runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// 1) client_addr + miner_addr statistics (store list into key: stats:client:<client_addr>)
	if err := computeAndStoreClientMiner(ctx); err != nil {
		log.Printf("[cron] client+miner agg error: %v", err)
	} else {
		log.Println("[cron] client+miner agg ok")
	}

	// 2) miner_addr statistics (store object into key: stats:miner:<miner>, and update ZSET)
	if err := computeAndStoreMiner(ctx); err != nil {
		log.Printf("[cron] miner agg error: %v", err)
	} else {
		log.Println("[cron] miner agg ok")
	}
}

// ============= Aggregations =============

// client_addr + miner_addr
func computeAndStoreClientMiner(ctx context.Context) error {
	// Count only module=http; success rate = success(true)/total
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"task.module": "http",
			// Time window (enable if needed)
			// "created_at": bson.M{"$gte": time.Now().Add(-24 * time.Hour)},
		}}},
		{{Key: "$group", Value: bson.M{
			"_id": bson.M{
				"client": "$task.metadata.client",
				"miner":  "$task.provider.id",
			},
			"total": bson.M{"$sum": 1},
			"ok":    bson.M{"$sum": bson.M{"$cond": []any{"$result.success", 1, 0}}},
		}}},
	}

	cur, err := colResult.Aggregate(ctx, pipeline, options.Aggregate().SetAllowDiskUse(true))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	// Build map: client -> []items
	group := make(map[string][]ClientMinerItem, 40000)
	for cur.Next(ctx) {
		var a aggOut2Keys
		if err := cur.Decode(&a); err != nil {
			return err
		}
		if a.ID.Client == "" || a.ID.Miner == "" || a.Total == 0 {
			continue
		}
		r := float64(a.OK) / float64(a.Total)
		it := ClientMinerItem{
			ClientAddr:           a.ID.Client,
			MinerAddr:            a.ID.Miner,
			SuccessRateHTTP:      r,
			SuccessRateGraphsync: 0,
			SuccessRateBitswap:   0,
		}
		group[a.ID.Client] = append(group[a.ID.Client], it)
	}
	if err := cur.Err(); err != nil {
		return err
	}

	// Write back to Redis: one client = one key (value is a JSON array)
	pipe := rds.Pipeline()
	for client, list := range group {
		// For UI convenience, store sorted by HTTP success rate (desc)
		sort.Slice(list, func(i, j int) bool { return list[i].SuccessRateHTTP > list[j].SuccessRateHTTP })
		bz, _ := json.Marshal(list)
		pipe.Set(ctx, keyClientPrefix+client, string(bz), redisTTL)
	}
	_, err = pipe.Exec(ctx)
	return err
}

// miner_addr
func computeAndStoreMiner(ctx context.Context) error {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"task.module": "http",
			// "created_at": bson.M{"$gte": time.Now().Add(-24 * time.Hour)},
		}}},
		{{Key: "$group", Value: bson.M{
			"_id":   "$task.provider.id",
			"total": bson.M{"$sum": 1},
			"ok":    bson.M{"$sum": bson.M{"$cond": []any{"$result.success", 1, 0}}},
		}}},
	}

	cur, err := colResult.Aggregate(ctx, pipeline, options.Aggregate().SetAllowDiskUse(true))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	pipe := rds.Pipeline()
	pipe.Del(ctx, zsetMinerHTTP) // Rebuild the index; differential updates are also possible
	for cur.Next(ctx) {
		var a aggOut1Key
		if err := cur.Decode(&a); err != nil {
			return err
		}
		if a.ID == "" || a.Total == 0 {
			continue
		}
		r := float64(a.OK) / float64(a.Total)
		doc := RateDoc{SuccessRateHTTP: r, SuccessRateGraphsync: 0, SuccessRateBitswap: 0}
		bz, _ := json.Marshal(doc)
		pipe.Set(ctx, keyMinerPrefix+a.ID, string(bz), redisTTL)
		pipe.ZAdd(ctx, zsetMinerHTTP, redis.Z{Member: a.ID, Score: r})
	}
	if err := cur.Err(); err != nil {
		return err
	}
	_, err = pipe.Exec(ctx)
	return err
}

// ============= HTTP =============

// /miners?miner_addr=&page=&page_size=
// - If miner_addr is provided: return only that miner (no pagination)
// - Otherwise: paginate from ZSET sorted by HTTP success rate (desc)
func handleMiners(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	minerQ := q.Get("miner_addr")

	// Pagination parameters
	page, pageSize := parsePage(q.Get("page"), q.Get("page_size"))
	start := int64((page - 1) * pageSize)
	end := start + int64(pageSize) - 1

	// No query provided: use the original efficient path
	if minerQ == "" {
		ids, err := rds.ZRevRange(ctx, zsetMinerHTTP, start, end).Result()
		if err != nil {
			http.Error(w, "redis zset error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		items := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			val, err := rds.Get(ctx, keyMinerPrefix+id).Result()
			if err != nil {
				if errors.Is(err, redis.Nil) {
					continue
				}
				http.Error(w, "redis get error: "+err.Error(), http.StatusInternalServerError)
				return
			}
			var rd RateDoc
			_ = json.Unmarshal([]byte(val), &rd)
			items = append(items, map[string]string{
				"miner_id":               id,
				"success_rate_http":      pct(rd.SuccessRateHTTP),
				"success_rate_graphsync": pct(rd.SuccessRateGraphsync),
				"success_rate_bitswap":   pct(rd.SuccessRateBitswap),
			})
		}
		// Total count
		total, _ := rds.ZCard(ctx, zsetMinerHTTP).Result()
		writeJSON(w, map[string]any{
			"page":      page,
			"page_size": pageSize,
			"total":     total,
			"items":     items,
		})
		return
	}

	// With miner_addr: fuzzy match (*keyword*), use ZSCAN to scan candidates, then sort by score descending and paginate
	type ms struct {
		id    string
		score float64
	}

	pattern := "*" + minerQ + "*"
	var cursor uint64
	var matched []ms

	for {
		// ZSCAN returns alternating [member, score, member, score, ...]
		keys, next, err := rds.ZScan(ctx, zsetMinerHTTP, cursor, pattern, 1000).Result()
		if err != nil {
			http.Error(w, "redis zscan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Parse alternating pairs
		for i := 0; i+1 < len(keys); i += 2 {
			member := keys[i]
			scoreStr := keys[i+1]
			sc, _ := strconv.ParseFloat(scoreStr, 64)
			matched = append(matched, ms{id: member, score: sc})
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	// Sort by score descending
	sort.Slice(matched, func(i, j int) bool { return matched[i].score > matched[j].score })

	total := int64(len(matched))
	if start >= total {
		writeJSON(w, map[string]any{
			"page":      page,
			"page_size": pageSize,
			"total":     total,
			"items":     []any{},
		})
		return
	}

	// Get current page
	endIdx := start + int64(pageSize)
	if endIdx > total {
		endIdx = total
	}
	pageMs := matched[start:endIdx]

	items := make([]map[string]string, 0, len(pageMs))
	for _, it := range pageMs {
		val, err := rds.Get(ctx, keyMinerPrefix+it.id).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			http.Error(w, "redis get error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		var rd RateDoc
		_ = json.Unmarshal([]byte(val), &rd)
		items = append(items, map[string]string{
			"miner_id":               it.id,
			"success_rate_http":      pct(rd.SuccessRateHTTP),
			"success_rate_graphsync": pct(rd.SuccessRateGraphsync),
			"success_rate_bitswap":   pct(rd.SuccessRateBitswap),
		})
	}

	writeJSON(w, map[string]any{
		"page":      page,
		"page_size": pageSize,
		"total":     total, // Total count of fuzzy matches
		"items":     items,
	})
}

// /clients?client_addr=&page=&page_size=
// - client_addr is required
// - Read JSON array from Redis key stats:client:<client_addr>
// - Sort by HTTP success rate (desc) again for safety, then paginate and return
func handleClients(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	client := q.Get("client_addr")
	if client == "" {
		http.Error(w, "client_addr is required", http.StatusBadRequest)
		return
	}

	val, err := rds.Get(ctx, keyClientPrefix+client).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			writeJSON(w, map[string]any{"count": 0, "items": []any{}})
			return
		}
		http.Error(w, "redis error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var list []ClientMinerItem
	if err := json.Unmarshal([]byte(val), &list); err != nil {
		http.Error(w, "decode error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Ensure descending order by HTTP success rate
	sort.Slice(list, func(i, j int) bool { return list[i].SuccessRateHTTP > list[j].SuccessRateHTTP })

	page, pageSize := parsePage(q.Get("page"), q.Get("page_size"))
	start := (page - 1) * pageSize
	if start >= len(list) {
		writeJSON(w, map[string]any{
			"page":      page,
			"page_size": pageSize,
			"total":     len(list),
			"items":     []any{},
		})
		return
	}
	end := start + pageSize
	if end > len(list) {
		end = len(list)
	}
	sub := list[start:end]

	items := make([]map[string]string, 0, len(sub))
	for _, it := range sub {
		items = append(items, map[string]string{
			"client_id":              it.ClientAddr,
			"miner_id":               it.MinerAddr,
			"success_rate_http":      pct(it.SuccessRateHTTP),
			"success_rate_graphsync": pct(it.SuccessRateGraphsync),
			"success_rate_bitswap":   pct(it.SuccessRateBitswap),
		})
	}

	writeJSON(w, map[string]any{
		"page":      page,
		"page_size": pageSize,
		"total":     len(list),
		"items":     items,
	})
}

// /details?miner_addr=...|client_addr=...&status=0|1&retrieval_method=http&page=&page_size=
func handleDetails(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	method := q.Get("retrieval_method")
	if method == "" {
		method = "http"
	}
	if method != "http" {
		http.Error(w, "only http supported", http.StatusBadRequest)
		return
	}

	filter := bson.M{"task.module": method}
	if miner := q.Get("miner_addr"); miner != "" {
		filter["task.provider.id"] = miner
	}
	if client := q.Get("client_addr"); client != "" {
		filter["task.metadata.client"] = client
	}
	if status := q.Get("status"); status != "" {
		switch status {
		case "0":
			filter["result.success"] = true
		case "1":
			filter["result.success"] = false
		default:
			http.Error(w, "status must be 0 or 1", http.StatusBadRequest)
			return
		}
	}

	page, pageSize := parsePage(q.Get("page"), q.Get("page_size"))
	skip := int64((page - 1) * pageSize)
	limit := int64(pageSize)

	// First get the total count
	total, err := colResult.CountDocuments(ctx, filter)
	if err != nil {
		http.Error(w, "mongo count error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(skip).
		SetLimit(limit)

	cur, err := colResult.Find(ctx, filter, opts)
	if err != nil {
		http.Error(w, "mongo find error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer cur.Close(ctx)

	type Row struct {
		MinerID         string      `json:"miner_id"`
		CID             string      `json:"cid"`
		Status          bool        `json:"status"`
		ReturnCode      string      `json:"return_code"`
		ResponseMessage string      `json:"response_message"`
		CreationTime    interface{} `json:"creation_time"`
	}

	var items []Row
	for cur.Next(ctx) {
		var m bson.M
		if err := cur.Decode(&m); err != nil {
			http.Error(w, "decode error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		items = append(items, Row{
			MinerID:         getString(m, "task", "provider", "id"),
			CID:             getString(m, "task", "content", "cid"),
			Status:          getBool(m, "result", "success"),
			ReturnCode:      getString(m, "result", "error_code"),
			ResponseMessage: getString(m, "result", "error_message"),
			CreationTime:    m["created_at"],
		})
	}
	if err := cur.Err(); err != nil {
		http.Error(w, "cursor error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"page":      page,
		"page_size": pageSize,
		"count":     total, // Use total count from database
		"items":     items, // Current page data
	})
}

// ============= utils =============

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		log.Fatalf("atoi %q: %v", s, err)
	}
	return n
}
func pct(f float64) string { return fmt.Sprintf("%.2f%%", f*100) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func parsePage(pStr, psStr string) (int, int) {
	page := 1
	if v, err := strconv.Atoi(pStr); err == nil && v > 0 {
		page = v
	}
	ps := defaultPageSize
	if v, err := strconv.Atoi(psStr); err == nil && v > 0 && v <= maxPageSize {
		ps = v
	}
	return page, ps
}

func getString(m bson.M, path ...string) string {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(bson.M)
		if !ok {
			return ""
		}
		cur = mm[p]
	}
	if s, ok := cur.(string); ok {
		return s
	}
	return ""
}
func getBool(m bson.M, path ...string) bool {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(bson.M)
		if !ok {
			return false
		}
		cur = mm[p]
	}
	if b, ok := cur.(bool); ok {
		return b
	}
	return false
}

// CORS middleware
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	mustInit()
	startCron()

	mux := http.NewServeMux()
	mux.HandleFunc("/miners", handleMiners)
	mux.HandleFunc("/clients", handleClients)
	mux.HandleFunc("/details", handleDetails)

	log.Printf("listening on %s", cfg.BindAddr)
	log.Fatal(http.ListenAndServe(cfg.BindAddr, withCORS(mux)))
}
