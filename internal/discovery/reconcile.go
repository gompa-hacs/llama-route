package discovery

import (
	"sort"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// Reconcile turns peer offers into pools, peer routes, and listings.
// Bare IDs already owned by local config (model IDs or aliases) are not registered.
func Reconcile(offers []Offer, cfg config.Config) RoutePlan {
	plan := RoutePlan{
		PeerRoutes:  make(map[string]PeerRoute),
		PoolAliases: make(map[string]int),
	}
	if len(offers) == 0 {
		return plan
	}

	// modelID -> contextSize -> offers
	byModel := make(map[string]map[int][]Offer)
	for _, o := range offers {
		if o.ModelID == "" || o.PeerID == "" {
			continue
		}
		ctxMap, ok := byModel[o.ModelID]
		if !ok {
			ctxMap = make(map[int][]Offer)
			byModel[o.ModelID] = ctxMap
		}
		ctxMap[o.ContextSize] = append(ctxMap[o.ContextSize], o)
	}

	modelIDs := make([]string, 0, len(byModel))
	for id := range byModel {
		modelIDs = append(modelIDs, id)
	}
	sort.Strings(modelIDs)

	for _, modelID := range modelIDs {
		ctxMap := byModel[modelID]
		ctxSizes := make([]int, 0, len(ctxMap))
		for cs := range ctxMap {
			ctxSizes = append(ctxSizes, cs)
		}
		sort.Ints(ctxSizes)

		_, localOwnsBare := cfg.RealModelName(modelID)
		allowBare := len(ctxSizes) == 1 && !localOwnsBare

		for _, cs := range ctxSizes {
			group := ctxMap[cs]
			sort.Slice(group, func(i, j int) bool {
				return group[i].PeerID < group[j].PeerID
			})

			if len(group) >= 2 {
				pool := buildPool(modelID, cs, group, allowBare)
				idx := len(plan.Pools)
				plan.Pools = append(plan.Pools, pool)
				for _, key := range pool.RouteKeys {
					plan.PoolAliases[key] = idx
				}
				// List bare when unambiguous; otherwise list FQ keys only.
				// FQ route keys stay registered for addressing but are not duplicated
				// in /v1/models when a bare name is already listed.
				if allowBare {
					plan.Listings = append(plan.Listings, Listing{
						ID:              modelID,
						PeerID:          "",
						UpstreamModelID: modelID,
						ContextSize:     cs,
						Pooled:          true,
						Discovered:      true,
					})
				} else {
					for _, key := range pool.RouteKeys {
						plan.Listings = append(plan.Listings, Listing{
							ID:              key,
							PeerID:          fqPeerID(key, modelID),
							UpstreamModelID: modelID,
							ContextSize:     cs,
							Pooled:          true,
							Discovered:      true,
						})
					}
				}
				continue
			}

			// Single offer → peer route (+ FQ always; bare if allowed).
			o := group[0]
			fq := FQName(o.PeerID, o.ModelID)
			route := PeerRoute{
				PeerID:          o.PeerID,
				UpstreamModelID: o.ModelID,
				Proxy:           o.Proxy,
				ApiKey:          o.ApiKey,
				Filters:         o.Filters,
				ContextSize:     o.ContextSize,
			}
			plan.PeerRoutes[fq] = route
			if allowBare {
				plan.PeerRoutes[modelID] = route
				plan.Listings = append(plan.Listings, Listing{
					ID:              modelID,
					PeerID:          o.PeerID,
					UpstreamModelID: o.ModelID,
					ContextSize:     o.ContextSize,
					Pooled:          false,
					Discovered:      true,
				})
			} else {
				plan.Listings = append(plan.Listings, Listing{
					ID:              fq,
					PeerID:          o.PeerID,
					UpstreamModelID: o.ModelID,
					ContextSize:     o.ContextSize,
					Pooled:          false,
					Discovered:      true,
				})
			}
		}
	}

	sort.Slice(plan.Listings, func(i, j int) bool {
		return plan.Listings[i].ID < plan.Listings[j].ID
	})
	return plan
}

func buildPool(modelID string, cs int, group []Offer, allowBare bool) DiscoveredPool {
	backends := make([]DiscoveredBackend, 0, len(group))
	routeKeys := make([]string, 0, len(group)+1)
	for _, o := range group {
		backends = append(backends, DiscoveredBackend{
			PeerID:          o.PeerID,
			Proxy:           o.Proxy,
			ContextSize:     cs,
			ApiKey:          o.ApiKey,
			UpstreamModelID: modelID,
		})
		routeKeys = append(routeKeys, FQName(o.PeerID, modelID))
	}
	canonical := routeKeys[0]
	if allowBare {
		routeKeys = append([]string{modelID}, routeKeys...)
		canonical = modelID
	}
	return DiscoveredPool{
		CanonicalID:     canonical,
		UpstreamModelID: modelID,
		RouteKeys:       routeKeys,
		Backends:        backends,
	}
}

func fqPeerID(fq, modelID string) string {
	suffix := "/" + modelID
	if len(fq) > len(suffix) && fq[len(fq)-len(suffix):] == suffix {
		return fq[:len(fq)-len(suffix)]
	}
	return ""
}
