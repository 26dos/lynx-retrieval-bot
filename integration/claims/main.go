// main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"github.com/filecoin-project/go-address"
	lotusapi "github.com/filecoin-project/lotus/api"
	lotusclient "github.com/filecoin-project/lotus/api/client"
	"github.com/filecoin-project/lotus/api/v1api"
	"github.com/filecoin-project/lotus/chain/types"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.uber.org/zap"
)

/********** Logging **********/
var log *zap.SugaredLogger

/********** Config **********/
type cfg struct {
	LotusURL      string
	LotusJWT      string
	MongoURI      string
	MongoDB       string
	MongoColl     string
	DumpDir       string // directory that contains all_claims_YYYYMMDD.json
	BulkSize      int
	RunEveryHours int
}

func mustEnv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		if def != "" {
			return def
		}
		log.Fatalf("missing env %s", key)
	}
	return v
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var out int
	_, err := fmt.Sscan(v, &out)
	if err != nil {
		return def
	}
	return out
}

func loadCfg() cfg {
	return cfg{
		LotusURL:      mustEnv("FULLNODE_API_URL", ""),
		LotusJWT:      os.Getenv("FULLNODE_API_TOKEN"),
		MongoURI:      mustEnv("MONGO_URI", ""),
		MongoDB:       mustEnv("MONGO_DB", "filstats"),
		MongoColl:     mustEnv("MONGO_CLAIMS_COLL", "claims"),
		DumpDir:       os.Getenv("CLAIMS_DUMP_DIR"),
		BulkSize:      envInt("CLAIMS_BULK_SIZE", 2000),
		RunEveryHours: envInt("RUN_EVERY_HOURS", 1),
	}
}

/********** Mongo document schema **********/
type DBClaim struct {
	ClaimID    int64          `bson:"claim_id,omitempty"`
	ProviderID int64          `bson:"provider_id"`
	ClientID   int64          `bson:"client_id,omitempty"`
	ClientAddr string         `bson:"client_addr,omitempty"`
	DataCID    string         `bson:"data_cid"`
	Size       int64          `bson:"size"`
	TermMin    int64          `bson:"term_min"`
	TermMax    int64          `bson:"term_max"`
	TermStart  int64          `bson:"term_start"`
	Sector     uint64         `bson:"sector"`
	MinerAddr  string         `bson:"miner_addr,omitempty"`
	UpdatedAt  time.Time      `bson:"updated_at"`
	Meta       map[string]any `bson:"meta,omitempty"`
}

/********** Lotus connection **********/
func connectLotus(ctx context.Context, url, jwt string) (v1api.FullNode, func(), error) {
	hdr := http.Header{}
	if jwt != "" {
		hdr.Set("Authorization", "Bearer "+jwt)
	}
	full, closer, err := lotusclient.NewFullNodeRPCV1(ctx, url, hdr)
	if err != nil {
		return nil, func() {}, fmt.Errorf("connect lotus: %w", err)
	}
	return full, func() { closer() }, nil
}

/********** Mongo connection & indexes **********/
func connectMongo(ctx context.Context, uri, db, coll string) (*mongo.Client, *mongo.Collection, error) {
	mc, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, nil, err
	}
	c := mc.Database(db).Collection(coll)

	// Business unique key: (provider_id, data_cid, sector, term_start)
	_, _ = c.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "provider_id", Value: 1}, {Key: "data_cid", Value: 1}, {Key: "sector", Value: 1}, {Key: "term_start", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("uniq_claim_tuple"),
	})
	// Optional: claim_id unique (if present)
	_, _ = c.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "provider_id", Value: 1}, {Key: "claim_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetSparse(true).SetName("uniq_provider_claimid"),
	})
	// Auxiliary indexes
	_, _ = c.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "client_addr", Value: 1}}},
		{Keys: bson.D{{Key: "miner_addr", Value: 1}}},
		{Keys: bson.D{{Key: "updated_at", Value: -1}}},
	})

	return mc, c, nil
}

/********** Utilities **********/
func claimKey(providerID int64, dataCID string, sector uint64, termStart int64) string {
	return fmt.Sprintf("%d|%s|%d|%d", providerID, dataCID, sector, termStart)
}

func hasNonZeroPower(p *lotusapi.MinerPower) bool {
	if p == nil {
		return false
	}
	return p.MinerPower.RawBytePower.GreaterThan(types.NewInt(0)) ||
		p.MinerPower.QualityAdjPower.GreaterThan(types.NewInt(0))
}

/********** Load “active providers” (ActorID set) from Lotus **********/
func loadActiveProviders(ctx context.Context, api v1api.FullNode) (map[uint64]struct{}, error) {
	active := make(map[uint64]struct{}, 16384)

	head, err := api.ChainHead(ctx)
	if err != nil {
		return nil, fmt.Errorf("ChainHead: %w", err)
	}
	tsk := head.Key()

	miners, err := api.StateListMiners(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("StateListMiners: %w", err)
	}

	for _, m := range miners {
		mp, err := api.StateMinerPower(ctx, m, tsk)
		if err != nil {
			continue
		}
		if !hasNonZeroPower(mp) {
			continue
		}
		idAddr, err := api.StateLookupID(ctx, m, tsk)
		if err != nil {
			continue
		}
		id, err := address.IDFromAddress(idAddr)
		if err != nil {
			continue
		}
		active[uint64(id)] = struct{}{}
	}
	log.Infow("active providers loaded", "count", len(active))
	return active, nil
}

/********** Read all “business unique keys” from DB **********/
func loadAllClaimKeysFromDB(ctx context.Context, coll *mongo.Collection) (map[string]struct{}, error) {
	keys := make(map[string]struct{}, 1_000_000)

	cur, err := coll.Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{
		"provider_id": 1,
		"data_cid":    1,
		"sector":      1,
		"term_start":  1,
		"_id":         0,
	}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	type kdoc struct {
		ProviderID int64  `bson:"provider_id"`
		DataCID    string `bson:"data_cid"`
		Sector     uint64 `bson:"sector"`
		TermStart  int64  `bson:"term_start"`
	}

	for cur.Next(ctx) {
		var d kdoc
		if err := cur.Decode(&d); err != nil {
			return nil, err
		}
		k := claimKey(d.ProviderID, d.DataCID, d.Sector, d.TermStart)
		keys[k] = struct{}{}
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

/********** Lenient JSON helpers (support Data as object or string; numbers as string or number) **********/
type cidOrObj string

func (c *cidOrObj) UnmarshalJSON(b []byte) error {
	// 1) Plain string: "bafy..."
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = cidOrObj(s)
		return nil
	}
	// 2) Object: {"/":"bafy..."}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if v, ok := m["/"]; ok {
		if s, ok2 := v.(string); ok2 {
			*c = cidOrObj(s)
			return nil
		}
	}
	return fmt.Errorf("unsupported CID JSON format: %s", string(b))
}

type u64OrStr uint64

func (u *u64OrStr) UnmarshalJSON(b []byte) error {
	// Number
	if len(b) > 0 && b[0] != '"' {
		var x uint64
		if err := json.Unmarshal(b, &x); err != nil {
			return err
		}
		*u = u64OrStr(x)
		return nil
	}
	// String
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	x, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return err
	}
	*u = u64OrStr(x)
	return nil
}

type i64OrStr int64

func (i *i64OrStr) UnmarshalJSON(b []byte) error {
	// Number
	if len(b) > 0 && b[0] != '"' {
		var x int64
		if err := json.Unmarshal(b, &x); err != nil {
			return err
		}
		*i = i64OrStr(x)
		return nil
	}
	// String
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	x, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*i = i64OrStr(x)
	return nil
}

/********** Parse all_claims_YYYYMMDD.json and filter by “active providers” **********/
type filecoinClaim struct {
	Provider  u64OrStr `json:"Provider"`
	Client    u64OrStr `json:"Client"`
	Data      cidOrObj `json:"Data"` // lenient: string or {"/": "..."}
	Size      i64OrStr `json:"Size"`
	TermMin   i64OrStr `json:"TermMin"`
	TermMax   i64OrStr `json:"TermMax"`
	TermStart i64OrStr `json:"TermStart"`
	Sector    u64OrStr `json:"Sector"`
}

type rpcAllClaims struct {
	JSONRPC string                   `json:"jsonrpc"`
	Result  map[string]filecoinClaim `json:"result"`
	ID      any                      `json:"id"`
}

func loadClaimsFromFileFiltered(path string, active map[uint64]struct{}) ([]DBClaim, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var rpc rpcAllClaims
	dec := json.NewDecoder(f)
	if err := dec.Decode(&rpc); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}

	now := time.Now()
	out := make([]DBClaim, 0, len(rpc.Result))
	for claimIDStr, c := range rpc.Result {
		// Keep only providers that currently have power
		if _, ok := active[uint64(c.Provider)]; !ok {
			continue
		}
		var claimID int64
		_, _ = fmt.Sscan(claimIDStr, &claimID)

		out = append(out, DBClaim{
			ClaimID:    claimID,
			ProviderID: int64(c.Provider),
			ClientID:   int64(c.Client),
			DataCID:    string(c.Data), // convert from cidOrObj to string
			Size:       int64(c.Size),
			TermMin:    int64(c.TermMin),
			TermMax:    int64(c.TermMax),
			TermStart:  int64(c.TermStart),
			Sector:     uint64(c.Sector),
			MinerAddr:  fmt.Sprintf("f0%d", uint64(c.Provider)),
			UpdatedAt:  now,
		})
	}
	return out, nil
}

/********** Insert the set difference (no total cap; batched BulkWrite) **********/
func insertDiffClaims(ctx context.Context, coll *mongo.Collection, chainClaims []DBClaim, existingKeys map[string]struct{}, bulkSize int) (int64, error) {
	if len(chainClaims) == 0 {
		return 0, nil
	}
	if bulkSize <= 0 {
		bulkSize = 2000
	}

	var (
		batch      []mongo.WriteModel
		inserted   int64
		prepared   int64
		now        = time.Now()
		flushBatch = func() error {
			if len(batch) == 0 {
				return nil
			}
			res, err := coll.BulkWrite(ctx, batch, options.BulkWrite().SetOrdered(false))
			batch = batch[:0]
			if err != nil {
				// Allow partial success; conservatively count UpsertedCount
				log.Warnw("BulkWrite returned error (partial success possible)", "err", err)
			}
			if res != nil {
				inserted += res.UpsertedCount
			}
			return nil
		}
	)

	for _, c := range chainClaims {
		k := claimKey(c.ProviderID, c.DataCID, c.Sector, c.TermStart)
		if _, ok := existingKeys[k]; ok {
			continue // already exists
		}
		c.UpdatedAt = now
		filter := bson.M{
			"provider_id": c.ProviderID,
			"data_cid":    c.DataCID,
			"sector":      c.Sector,
			"term_start":  c.TermStart,
		}
		update := bson.M{"$setOnInsert": c}
		batch = append(batch, mongo.NewUpdateOneModel().SetFilter(filter).SetUpdate(update).SetUpsert(true))
		prepared++

		if len(batch) >= bulkSize {
			if err := flushBatch(); err != nil {
				return inserted, err
			}
		}
	}
	if err := flushBatch(); err != nil {
		return inserted, err
	}

	log.Infow("diff insert finished", "prepared", prepared, "upserted", inserted, "bulkSize", bulkSize)
	return inserted, nil
}

/********** Single run: ensure the dump file exists and is stable, then proceed **********/
func runFromTodayDumpOnce(ctx context.Context, api v1api.FullNode, coll *mongo.Collection, dumpDir string, bulkSize int) error {
	startAt := time.Now()
	log.Infow("run start", "start_at", startAt.Format(time.RFC3339))

	dateStr := time.Now().Format("20060102")
	if dumpDir == "" {
		dumpDir = "."
	}
	filePath := filepath.Join(dumpDir, fmt.Sprintf("all_claims_%s.json", dateStr))

	// 1) Check file existence
	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Infow("dump file not found, skip this run (early return)", "file", filePath)
			return nil
		}
		return fmt.Errorf("stat dump file: %w", err)
	}

	// 2) Check if the file is still being written (size stability)
	const stableCheckInterval = 5 * time.Second
	const stableCheckRetries = 3

	stable := false
	prevSize := info.Size()
	for i := 0; i < stableCheckRetries; i++ {
		time.Sleep(stableCheckInterval)
		info2, err := os.Stat(filePath)
		if err != nil {
			return fmt.Errorf("stat dump file during stability check: %w", err)
		}
		if info2.Size() == prevSize {
			stable = true
			break
		}
		log.Infow("dump file still growing, wait more...",
			"file", filePath,
			"prev_size", prevSize,
			"new_size", info2.Size(),
			"retry", i+1)
		prevSize = info2.Size()
	}
	if !stable {
		log.Warnw("dump file not stable, skip this run", "file", filePath)
		return nil
	}
	log.Infow("using stable dump file", "file", filePath)

	// 3) Load active providers
	active, err := loadActiveProviders(ctx, api)
	if err != nil {
		return fmt.Errorf("load active providers: %w", err)
	}
	if len(active) == 0 {
		log.Warn("no active providers found; nothing to do")
		return nil
	}

	// 4) Load from file + filter
	claimsList, err := loadClaimsFromFileFiltered(filePath, active)
	if err != nil {
		return err
	}
	log.Infow("claims loaded from file (filtered by active providers)", "count", len(claimsList))

	// 5) Load existing DB key set
	existingKeys, err := loadAllClaimKeysFromDB(ctx, coll)
	if err != nil {
		return fmt.Errorf("load db keys: %w", err)
	}
	log.Infow("loaded db claim keys", "count", len(existingKeys))

	// 6) Upsert the set difference
	added, err := insertDiffClaims(ctx, coll, claimsList, existingKeys, bulkSize)
	if err != nil {
		return err
	}

	// 7) Remove the dump file after ingest
	if err := os.Remove(filePath); err != nil {
		log.Warnw("failed to remove dump file", "file", filePath, "err", err)
	} else {
		log.Infow("dump file removed", "file", filePath)
	}

	endAt := time.Now()
	log.Infow("run end",
		"end_at", endAt.Format(time.RFC3339),
		"took", endAt.Sub(startAt).String(),
		"added", added,
	)
	return nil
}

/********** main: run every N hours **********/
func main() {
	// Initialize zap
	zlogger, _ := zap.NewProduction()
	defer zlogger.Sync()
	log = zlogger.Sugar()

	cfg := loadCfg()
	log.Infow("boot",
		"lotus", cfg.LotusURL,
		"mongo", cfg.MongoURI,
		"db", cfg.MongoDB, "coll", cfg.MongoColl,
		"dumpDir", cfg.DumpDir,
		"bulkSize", cfg.BulkSize,
		"runEveryHours", cfg.RunEveryHours,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// lotus
	full, closeLotus, err := connectLotus(ctx, cfg.LotusURL, cfg.LotusJWT)
	if err != nil {
		log.Fatalw("connect lotus failed", "err", err)
	}
	defer closeLotus()

	// mongo
	mc, claimsColl, err := connectMongo(ctx, cfg.MongoURI, cfg.MongoDB, cfg.MongoColl)
	if err != nil {
		log.Fatalw("connect mongo failed", "err", err)
	}
	defer mc.Disconnect(ctx)

	// Run once immediately
	if err := runFromTodayDumpOnce(ctx, full, claimsColl, cfg.DumpDir, cfg.BulkSize); err != nil {
		log.Errorw("first run failed", "err", err)
	}

	// Periodic run (default: hourly)
	interval := time.Duration(cfg.RunEveryHours)
	if interval <= 0 {
		interval = 1
	}
	ticker := time.NewTicker(interval * time.Hour)
	defer ticker.Stop()
	log.Infow("scheduler started", "interval_hours", interval)

	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return
		case <-ticker.C:
			if err := runFromTodayDumpOnce(ctx, full, claimsColl, cfg.DumpDir, cfg.BulkSize); err != nil {
				log.Errorw("scheduled run failed", "err", err)
			}
		}
	}
}
