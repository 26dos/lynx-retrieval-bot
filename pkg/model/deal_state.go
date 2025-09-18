package model

import (
	"time"
)

// -----------------------------
// Constants: Filecoin epoch ↔ Unix time conversion
// Mainnet genesis: 2020-08-25 22:00:00 UTC → 1598306400
// 1 epoch = 30 seconds
// -----------------------------
const (
	filecoinGenesisUnix = int64(1598306400)
	epochDurationSec    = int64(30)
)

// -----------------------------
// int32 versions (kept for backward compatibility)
// -----------------------------
func EpochToTime(epoch int32) time.Time {
	if epoch < 0 {
		return time.Time{}
	}
	return time.Unix(int64(epoch*30+1598306400), 0).UTC()
}

func TimeToEpoch(t time.Time) int32 {
	if t.IsZero() {
		return -1
	}
	return int32(t.Unix()-1598306400) / 30
}

// -----------------------------
// int64 versions (preferred)
// -----------------------------
func EpochToTime64(epoch int64) time.Time {
	if epoch < 0 {
		return time.Time{}
	}
	return time.Unix(epoch*epochDurationSec+filecoinGenesisUnix, 0).UTC()
}

func TimeToEpoch64(t time.Time) int64 {
	if t.IsZero() {
		return -1
	}
	return (t.UTC().Unix() - filecoinGenesisUnix) / epochDurationSec
}

// -----------------------------
// New model: DBClaim
// -----------------------------
type DBClaim struct {
	ClaimID    int64          `bson:"claim_id"`    // verifreg.ClaimId
	ProviderID int64          `bson:"provider_id"` // abi.ActorID
	ClientID   int64          `bson:"client_id"`   // abi.ActorID
	ClientAddr string         `bson:"client_addr"` // f1/f3... address
	DataCID    string         `bson:"data_cid"`    // CID string
	Size       int64          `bson:"size"`        // padded piece size (bytes)
	TermMin    int64          `bson:"term_min"`    // epochs
	TermMax    int64          `bson:"term_max"`    // epochs
	TermStart  int64          `bson:"term_start"`  // epoch
	Sector     uint64         `bson:"sector"`      // sector number
	MinerAddr  string         `bson:"miner_addr"`  // f0... miner ID address
	UpdatedAt  time.Time      `bson:"updated_at"`  // upsert timestamp (UTC)
	Meta       map[string]any `bson:"meta,omitempty"`
}

// Convenience: actual wall-clock time of TermStart
func (c DBClaim) TermStartTime() time.Time {
	return EpochToTime64(c.TermStart)
}

// Convenience: convert TermMin/TermMax to duration
func (c DBClaim) TermMinDuration() time.Duration {
	if c.TermMin <= 0 {
		return 0
	}
	return time.Duration(c.TermMin*epochDurationSec) * time.Second
}

func (c DBClaim) TermMaxDuration() time.Duration {
	if c.TermMax <= 0 {
		return 0
	}
	return time.Duration(c.TermMax*epochDurationSec) * time.Second
}

// Convenience: "age" from TermStart to now (in years)
func (c DBClaim) AgeInYears() float64 {
	ts := c.TermStartTime()
	if ts.IsZero() {
		return 0
	}
	return time.Since(ts).Hours() / 24.0 / 365.0
}

// Set UpdatedAt to current UTC time
func (c *DBClaim) Touch() {
	c.UpdatedAt = time.Now().UTC()
}
