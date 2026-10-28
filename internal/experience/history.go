package experience

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/simulate"
	serverapp "gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/rollup"
)

// seedHistory writes the month the platform has been in use: the requests
// (usage.go), every card's hourly utilization, memory, power and
// temperature, and the card time each deployment held, in the shapes the
// live statistics write them, up to the hour the environment started. It
// waits for the starters to run, since the history is theirs and the cards
// they hold.
func seedHistory(ctx context.Context, app *serverapp.App, c *apiClient, members map[string]member, keys map[string]string, marker string, log *slog.Logger) {
	if _, err := os.Stat(marker); err == nil {
		return
	}
	for _, st := range starters {
		if _, err := waitRunning(ctx, c, st.Name); err != nil {
			log.Warn("history waits for the starters", "err", err)
			return
		}
	}
	list, err := listDeployments(ctx, c)
	if err != nil {
		log.Warn("history skipped", "err", err)
		return
	}
	ids := map[string]uint{}
	for _, d := range list {
		ids[d.Name] = d.ID
	}
	stored, err := app.Store().ListAPIKeys()
	if err != nil {
		log.Warn("history skipped", "err", err)
		return
	}
	keyIDs := map[string][2]uint{}
	for _, k := range stored {
		if keys[k.Name] != "" {
			keyIDs[k.Name] = [2]uint{k.ID, k.UserID}
		}
	}
	cards, err := app.StatisticsCards()
	if err != nil {
		log.Warn("history skipped", "err", err)
		return
	}
	end := time.Now().Truncate(time.Hour) // the current hour is live
	start := end.AddDate(0, 0, -30)
	h := cardHistory(start, end, cards, ids, members)
	h.Requests, h.Weights = requestHistory(start, end, ids, keyIDs)
	if err := app.SeedHistory(ctx, h); err != nil {
		log.Warn("history not written", "err", err)
		return
	}
	_ = os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// samplesPerHour is how many samples a simulated node's card gives an hour
// (one every 30 s).
const samplesPerHour = 120

// cardHistory is every card's hours and the deployments' card time from
// start to end: a card a starter holds works as hard as the hour's load
// once the starter went live; the others rest.
func cardHistory(start, end time.Time, cards []serverapp.StatisticsCard, ids map[string]uint, members map[string]member) serverapp.History {
	h := serverapp.History{
		GPUs: map[rollup.GPUKey]rollup.GPU{}, GPUModels: map[rollup.GPUKey]string{},
		GPUTime: map[rollup.GPUTimeKey]rollup.GPUTime{}, Capacity: map[rollup.CapacityKey]rollup.Capacity{},
	}
	tdp := map[string]float64{}
	for _, n := range scenario {
		tdp[members[n.Hostname].UUID] = n.TDP
	}
	live := map[uint]time.Time{}
	replicas := map[uint]int{}
	for _, st := range starters {
		if id := ids[st.Name]; id != 0 {
			live[id], replicas[id] = end.AddDate(0, 0, -st.Days), st.Replicas
		}
	}
	rng := rand.New(rand.NewPCG(uint64(start.Unix()), 11))
	const n = samplesPerHour
	for _, card := range cards {
		named := card.Key
		named.HourMS = 0
		h.GPUModels[named] = card.Model
	}
	for hour := start; hour.Before(end); hour = hour.Add(time.Hour) {
		ms := hour.UnixMilli()
		busy := simulate.BusyUtilization(simulate.Rhythm(hour.Add(30 * time.Minute)))
		type held struct {
			gpu, whole float64
			model      string
		}
		holding := map[uint]*held{}
		for i, card := range cards {
			share := 0.0
			for dep, s := range card.Holders {
				if since, ok := live[dep]; ok && hour.Before(since) {
					continue
				}
				share += s
				hd := holding[dep]
				if hd == nil {
					hd = &held{model: card.Model}
					holding[dep] = hd
				}
				hd.gpu += s * 3600
				hd.whole += 3600
			}
			share = math.Min(1, share)
			util := 0.4 + rng.Float64()*0.6
			used := card.MemoryTotal * 0.006
			if share > 0 {
				util = busy*math.Min(1, 0.4+share) + float64((i*7)%9) - 4 + rng.NormFloat64()*3
				used = card.MemoryTotal * math.Min(0.97, share*0.895+0.004)
			}
			util = math.Min(math.Max(util, 0), 100)
			power := tdp[card.Key.NodeUUID] * (0.14 + 0.78*util/100)
			temp := 33 + 42*util/100
			key := card.Key
			key.HourMS = ms
			h.GPUs[key] = rollup.GPU{
				UtilSum: util * n, UtilN: n, UtilMax: math.Min(100, util+6+rng.Float64()*10),
				MemUsedSum: used * n, MemUsedN: n, MemUsedMax: used * 1.01, MemTotal: card.MemoryTotal,
				PowerSum: power * n, PowerN: n,
				TempSum: temp * n, TempN: n, TempMax: temp + 2 + rng.Float64()*3,
			}
			ck := rollup.CapacityKey{HourMS: ms, GPUModel: card.Model}
			c := h.Capacity[ck]
			c.Add(rollup.Capacity{TotalGPUSeconds: 3600, SchedulableGPUSeconds: 3600})
			h.Capacity[ck] = c
		}
		for dep, hd := range holding {
			k := rollup.GPUTimeKey{HourMS: ms, DeploymentID: dep, GPUModel: hd.model}
			r := int64(max(replicas[dep], 1))
			h.GPUTime[k] = rollup.GPUTime{GPUSeconds: hd.gpu, WholeGPUSeconds: hd.whole, InstanceSeconds: float64(r) * 3600, MaxInstances: r}
		}
	}
	return h
}
