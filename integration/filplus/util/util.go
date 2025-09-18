package util

import (
	"context"
	"time"

	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/pkg/errors"

	"storagestats/pkg/convert"
	"storagestats/pkg/env"
	"storagestats/pkg/model"
	"storagestats/pkg/requesterror"
	"storagestats/pkg/resolver"
	"storagestats/pkg/task"
)

var logger = logging.Logger("addTasks")

//nolint:nonamedreturns
func AddTasks(
	ctx context.Context,
	requester string,
	ipInfo resolver.IPInfo,
	documents []model.DBClaim,
	locationResolver resolver.LocationResolver,
	providerResolver resolver.ProviderResolver,
) (tasks []interface{}, results []interface{}) {
	for _, document := range documents {
		// Resolve provider (using DBClaim.MinerAddr: f0... miner ID address)
		providerInfo, err := providerResolver.ResolveProvider(ctx, document.MinerAddr)
		if err != nil {
			logger.With("provider", document.MinerAddr).
				Error("failed to resolve provider")
			continue
		}

		// Resolve multiaddrs
		location, err := locationResolver.ResolveMultiaddrsBytes(ctx, providerInfo.Multiaddrs)
		if err != nil {
			if errors.As(err, &requesterror.BogonIPError{}) ||
				errors.As(err, &requesterror.InvalidIPError{}) ||
				errors.As(err, &requesterror.HostLookupError{}) ||
				errors.As(err, &requesterror.NoValidMultiAddrError{}) {
				results = addErrorResults(requester, ipInfo, results, document, providerInfo, location,
					task.NoValidMultiAddrs, err.Error())
			} else {
				logger.With("provider", document.MinerAddr, "err", err).
					Error("failed to resolve provider location")
			}
			continue
		}

		// Validate PeerID
		_, err = peer.Decode(providerInfo.PeerId)
		if err != nil {
			logger.With("provider", document.MinerAddr, "peerID", providerInfo.PeerId, "err", err).
				Info("failed to decode peerID")
			results = addErrorResults(requester, ipInfo, results, document, providerInfo, location,
				task.InvalidPeerID, err.Error())
			continue
		}

		// Only add HTTP piece retrieval task (using DataCID)
		tasks = append(tasks, task.Task{
			Requester: requester,
			Module:    task.HTTP,
			Metadata: map[string]string{
				"client":        document.ClientAddr,
				"retrieve_type": "piece",
				"retrieve_size": "1048576",
			},
			Provider: task.Provider{
				ID:         document.MinerAddr,
				PeerID:     providerInfo.PeerId,
				Multiaddrs: convert.MultiaddrsBytesToStringArraySkippingError(providerInfo.Multiaddrs),
				City:       location.City,
				Region:     location.Region,
				Country:    location.Country,
				Continent:  location.Continent,
			},
			Content: task.Content{
				CID: document.DataCID,
			},
			CreatedAt: time.Now().UTC(),
			Timeout:   env.GetDuration(env.FilplusIntegrationTaskTimeout, 15*time.Second),
		})
	}

	logger.With("count", len(tasks)).Info("inserted tasks")
	//nolint:nakedret
	return
}

// Only keep metadata that matches the current logic (remove deal_id/label)
var moduleMetadataMap = map[task.ModuleName]map[string]string{
	task.GraphSync: {
		"assume_label":  "true",
		"retrieve_type": "root_block",
	},
	task.Bitswap: {
		"assume_label":  "true",
		"retrieve_type": "root_block",
	},
	task.HTTP: {
		"retrieve_type": "piece",
		"retrieve_size": "1048576",
	},
}

func addErrorResults(
	requester string,
	ipInfo resolver.IPInfo,
	results []interface{},
	document model.DBClaim,
	providerInfo resolver.MinerInfo,
	location resolver.IPInfo,
	errorCode task.ErrorCode,
	errorMessage string,
) []interface{} {
	for module, metadata := range moduleMetadataMap {
		newMetadata := make(map[string]string)
		for k, v := range metadata {
			newMetadata[k] = v
		}
		// No longer includes deal_id; client is changed to DBClaim.ClientAddr
		newMetadata["client"] = document.ClientAddr

		results = append(results, task.Result{
			Task: task.Task{
				Requester: requester,
				Module:    module,
				Metadata:  newMetadata,
				Provider: task.Provider{
					ID:         document.MinerAddr,
					PeerID:     providerInfo.PeerId,
					Multiaddrs: convert.MultiaddrsBytesToStringArraySkippingError(providerInfo.Multiaddrs),
					City:       location.City,
					Region:     location.Region,
					Country:    location.Country,
					Continent:  location.Continent,
				},
				Content: task.Content{
					// Always use DataCID
					CID: document.DataCID,
				},
				CreatedAt: time.Now().UTC(),
				Timeout:   env.GetDuration(env.FilplusIntegrationTaskTimeout, 15*time.Second),
			},
			Retriever: task.Retriever{
				PublicIP:  ipInfo.IP,
				City:      ipInfo.City,
				Region:    ipInfo.Region,
				Country:   ipInfo.Country,
				Continent: ipInfo.Continent,
				ASN:       ipInfo.ASN,
				ISP:       ipInfo.ISP,
				Latitude:  ipInfo.Latitude,
				Longitude: ipInfo.Longitude,
			},
			Result: task.RetrievalResult{
				Success:      false,
				ErrorCode:    errorCode,
				ErrorMessage: errorMessage,
				TTFB:         0,
				Speed:        0,
				Duration:     0,
				Downloaded:   0,
			},
			CreatedAt: time.Now().UTC(),
		})
	}
	return results
}
