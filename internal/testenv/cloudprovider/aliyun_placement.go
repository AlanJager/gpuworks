package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	aliyundriver "gpuworks.ai/gpuworks/internal/server/provider/aliyun"
)

func aliyunPlacementKey(model string, count int) string { return placementKey(model, count) }

// canonicalAliyunModel maps what a caller wrote ("L20", "NVIDIA L20",
// "Tesla T4") to the card name of the shape table.
func canonicalAliyunModel(model string) (string, bool) {
	upper := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "_", "").Replace(model))
	for _, known := range AliyunGPUModels() {
		if upper == known || strings.HasSuffix(upper, known) {
			return known, true
		}
	}
	return "", false
}

func (p *Aliyun) regionStock(ctx context.Context, region string, spot bool) (map[stockKey]string, error) {
	cacheKey := fmt.Sprintf("%s/%t", region, spot)
	p.mu.Lock()
	entry, ok := p.stockCache[cacheKey]
	p.mu.Unlock()
	if ok && p.now().Sub(entry.at) < aliyunStockTTL {
		return entry.stock, nil
	}
	stock, err := p.api.stock(ctx, region, spot)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.stockCache[cacheKey] = aliyunStockEntry{at: p.now(), stock: stock}
	p.mu.Unlock()
	return stock, nil
}

// SelectPlacement lists where gpuCount GPUs of one card can be bought right
// now, spot placements first and on-demand after them, and remembers the list
// for the following Rent. It returns the best placement.
func (p *Aliyun) SelectPlacement(ctx context.Context, gpuModel string, gpuCount int) (AliyunPlacement, error) {
	model, ok := canonicalAliyunModel(gpuModel)
	if !ok {
		return AliyunPlacement{}, fmt.Errorf("aliyun: unknown GPU %q (known: %s)", gpuModel, strings.Join(AliyunGPUModels(), ", "))
	}
	shapes := AliyunShapes(model, gpuCount)
	if len(shapes) == 0 {
		return AliyunPlacement{}, fmt.Errorf("aliyun: no %s shape with %d GPUs", model, gpuCount)
	}
	regions := p.rentRegions()
	if len(regions) == 0 {
		return AliyunPlacement{}, errors.New("aliyun: no golden image is configured for any region (ALIYUN_IMAGE_<REGION>)")
	}
	type modeStock struct {
		spot  map[string]map[stockKey]string
		onDem map[string]map[stockKey]string
	}
	collected := modeStock{spot: map[string]map[stockKey]string{}, onDem: map[string]map[stockKey]string{}}
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		slots  = make(chan struct{}, aliyunRegionConcurrency)
		errMsg []string
	)
	for _, region := range regions {
		for _, spot := range []bool{true, false} {
			if (spot && p.cfg.NoSpot) || (!spot && p.cfg.NoOnDemand) {
				continue
			}
			wg.Add(1)
			go func(region string, spot bool) {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()
				stock, err := p.regionStock(ctx, region, spot)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errMsg = append(errMsg, fmt.Sprintf("%s %s: %v", region, map[bool]string{true: "spot", false: "on-demand"}[spot], err))
					return
				}
				if spot {
					collected.spot[region] = stock
				} else {
					collected.onDem[region] = stock
				}
			}(region, spot)
		}
	}
	wg.Wait()
	sort.Strings(errMsg)

	var list []AliyunPlacement
	for _, spot := range []bool{true, false} {
		byRegion := collected.onDem
		if spot {
			byRegion = collected.spot
		}
		for _, shape := range shapes {
			for _, region := range regions {
				var zones []AliyunPlacement
				for key, category := range byRegion[region] {
					if key.instanceType == shape.InstanceType {
						zones = append(zones, AliyunPlacement{Region: region, Zone: key.zone, Shape: shape, Spot: spot, Stock: category})
					}
				}
				sort.Slice(zones, func(i, j int) bool {
					if (zones[i].Stock == stockWithStock) != (zones[j].Stock == stockWithStock) {
						return zones[i].Stock == stockWithStock
					}
					return zones[i].Zone < zones[j].Zone
				})
				list = append(list, zones...)
			}
		}
	}
	if len(list) == 0 {
		detail := ""
		if len(errMsg) > 0 {
			detail = "; stock query errors: " + strings.Join(errMsg, "; ")
		}
		return AliyunPlacement{}, fmt.Errorf("no aliyun capacity for %s x%d in %s (spot and on-demand)%s", model, gpuCount, strings.Join(regions, ", "), detail)
	}
	p.mu.Lock()
	p.candidates[aliyunPlacementKey(model, gpuCount)] = list
	p.mu.Unlock()
	return list[0], nil
}

// isAliyunCapacityError reports a refusal that another zone, type or billing
// mode can get around: no stock, a quota, a type the zone does not sell, or an
// operation the account's spot/disk entitlement denies. Anything else (bad
// credentials, unpaid balance, a missing image) is a defect or an account
// problem and must fail the case.
func isAliyunCapacityError(err error) bool {
	code := aliyundriver.ErrorCode(err)
	switch {
	case strings.Contains(code, "NoStock"):
		return true
	case strings.HasPrefix(code, "OperationDenied"), strings.HasPrefix(code, "QuotaExceed"),
		strings.HasPrefix(code, "InvalidResourceType"), strings.HasPrefix(code, "InvalidInstanceType"),
		strings.HasPrefix(code, "Zone.NotOnSale"), strings.HasPrefix(code, "ResourceNotAvailable"),
		strings.HasPrefix(code, "InvalidSystemDiskCategory"), strings.HasPrefix(code, "InvalidDiskCategory"),
		strings.HasPrefix(code, "InvalidSpotStrategy"), strings.HasPrefix(code, "SpotQuota"):
		return true
	}
	return false
}
