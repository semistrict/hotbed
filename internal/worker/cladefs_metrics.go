package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	cladefsdv1 "github.com/semistrict/cladefs/go/cladefsd/v1"
	"github.com/semistrict/hotbed/internal/cladefs"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// hostCladefsdMetricsURL is where the Linux host's cladefsd.service serves
// its Prometheus metrics.
const hostCladefsdMetricsURL = "http://127.0.0.1:9184/metrics"

// sampleCladefsObjectStore reads store's counters and gauges, and the
// Prometheus metrics at metricsURL unless it is empty.
func sampleCladefsObjectStore(ctx context.Context, store *cladefs.Store, metricsURL, peerAddr string, peers []string) *pb.ObjectStoreStats {
	if store == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := store.GetCounters(cctx)
	if err != nil {
		slog.Warn("sample cladefsd object-store counters", "err", err)
		return nil
	}
	out := objectStoreStatsFromCounters(resp)
	out.PeerAddress = peerAddr
	out.Peers = peers
	sctx, scancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer scancel()
	if stats, serr := store.GetStats(sctx, &cladefsdv1.GetStatsRequest{
		WaitMs:             500,
		LeaveOutHistograms: true,
	}); serr == nil {
		applyCladefsStats(out, stats)
	}
	if metricsURL != "" {
		scrapeLocalCladefsMetrics(sctx, metricsURL, out)
	}
	return out
}

func applyCladefsStats(out *pb.ObjectStoreStats, stats *cladefsdv1.GetStatsResponse) {
	if out == nil || stats == nil {
		return
	}
	g := stats.GetGauges()
	out.ExtentCacheBytes = g["memory_bytes"]
	out.ExtentCacheCapacityBytes = g["memory_capacity_bytes"]
	out.ExtentCacheLimitBytes = g["memory_limit_bytes"]
	out.ExtentCachePinnedEntries = g["extent_cache_pinned_entries"]
	out.ExtentCacheIdleEntries = g["extent_cache_idle_entries"]
	out.WindowMappedBytes = g["window_mapped_bytes"]
	out.OpenCells = uint32(g["stores_open"])
	out.HostDiskCellsBytes = g["disk_bytes"]
	if c := stats.GetCounters(); c != nil {
		out.CacheTrims = c["cache_trims"]
	}
	vms := stats.GetVms()
	out.ActiveVms = uint32(len(vms))
	for _, v := range vms {
		gc := v.GetGuest().GetCounters()
		if len(gc) == 0 {
			continue
		}
		out.DaxWindowFull += gc["window_full"]
		out.DaxWindowReclaims += gc["window_reclaims"]
		out.DaxRegionsFull += gc["window_regions_full"]
		out.DaxWritesHeldBySealsNs += gc["writes_held_by_seals_ns"]
		out.DaxDeviceReadBytes += gc["device_read_bytes"]
		out.DaxDeviceReadsNs += gc["device_reads_ns"]
	}
}

var cladefsMetricsHTTP = &http.Client{Timeout: 500 * time.Millisecond}

func scrapeLocalCladefsMetrics(ctx context.Context, url string, out *pb.ObjectStoreStats) bool {
	if out == nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := cladefsMetricsHTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return false
	}
	return applyCladefsPrometheusMetrics(out, string(body))
}

func applyCladefsPrometheusMetrics(out *pb.ObjectStoreStats, text string) bool {
	if out == nil {
		return false
	}
	var (
		matched          bool
		hostDiskSize     uint64
		hostDiskFree     uint64
		hostDiskCells    uint64
		hostDiskCache    uint64
		hostDiskOther    uint64
		uploadPending    uint64
		sawHostDiskCells bool
	)
	for _, line := range strings.Split(text, "\n") {
		name, labels, val, ok := parsePromLine(line)
		if !ok {
			continue
		}
		switch name {
		case "cladefsd_part_cache_bytes":
			out.PartCacheBytes = val
			matched = true
		case "cladefsd_part_cache_capacity_bytes":
			out.PartCacheCapacityBytes = val
			matched = true
		case "cladefsd_host_disk_size_bytes":
			hostDiskSize += val
			matched = true
		case "cladefsd_host_disk_free_bytes":
			hostDiskFree += val
			matched = true
		case "cladefsd_host_disk_stores_bytes":
			hostDiskCells += val
			sawHostDiskCells = true
			matched = true
		case "cladefsd_host_disk_cache_bytes":
			hostDiskCache += val
			matched = true
		case "cladefsd_host_disk_other_bytes":
			hostDiskOther += val
			matched = true
		case "cladefsd_host_disk_pressure":
			if val > 0 {
				out.HostDiskPressure = true
			}
			matched = true
		case "cladefsd_mount_cells":
			if labels == `state="attached"` {
				out.MountCellsAttached = uint32(val)
				matched = true
			}
		case "cladefsd_page_cache_bytes":
			out.PageCacheBytes = val
			matched = true
		case "cladefsd_page_cache_dirty_bytes":
			out.PageCacheDirtyBytes = val
			matched = true
		case "cladefsd_page_cache_limit_bytes":
			out.PageCacheLimitBytes = val
			matched = true
		case "cladefsd_part_cache_shard_state":
			out.PartCacheShards = append(out.PartCacheShards, partCacheShard(labels, val))
			matched = true
		case "cladefsd_peer_parts_total":
			switch promLabel(labels, "outcome") {
			case "given":
				out.PeerPartsGiven = val
			case "missing":
				out.PeerPartsMissing = val
			case "failed":
				out.PeerPartsFailed = val
			}
			matched = true
		case "cladefsd_peer_asked_parts_total":
			out.PeerPartsAsked = val
			matched = true
		case "cladefsd_peer_given_bytes_total":
			out.PeerGivenBytes = val
			matched = true
		case "cladefsd_peer_served_bytes_total":
			out.PeerServedBytes = val
			matched = true
		case "cladefsd_part_fetches_total", "cladefsd_part_fetch_bytes_total", "cladefsd_tail_fetches_total":
			if out.PartFetches == nil {
				out.PartFetches = &pb.PartFetches{}
			}
			applyPartFetch(out.PartFetches, name, promLabel(labels, "source"), val)
			matched = true
		case "cladefsd_peer_served_parts_total":
			switch promLabel(labels, "outcome") {
			case "served":
				out.PeerPartsServed = val
			case "missing":
				out.PeerServedPartsMissing = val
			}
			matched = true
		case "cladefsd_upload_pending_bytes":
			uploadPending += val
			matched = true
		case "cladefsd_memory_pressure":
			if val > 0 {
				out.MemoryPressure = true
			}
			matched = true
		case "cladefsd_memory_full":
			if val > 0 {
				out.MemoryFull = true
			}
			matched = true
		case "cladefsd_memory_limit_bytes":
			out.ExtentCacheLimitBytes = val
			matched = true
		case "cladefsd_memory_floor_bytes":
			out.ExtentCacheFloorBytes = val
			matched = true
		}
	}
	if hostDiskSize > 0 {
		out.HostDiskSizeBytes = hostDiskSize
		out.HostDiskFreeBytes = hostDiskFree
		out.HostDiskCacheBytes = hostDiskCache
		out.HostDiskOtherBytes = hostDiskOther
	}
	if sawHostDiskCells {
		out.HostDiskCellsBytes = hostDiskCells
	}
	if uploadPending > 0 {
		out.UploadPendingBytes = uploadPending
	}
	return matched
}

// applyPartFetch records one sample of cladefsd's part and tail fetch
// counters by source.
func applyPartFetch(f *pb.PartFetches, name, source string, val uint64) {
	var dst *uint64
	switch name + "/" + source {
	case "cladefsd_part_fetches_total/memory":
		dst = &f.Memory
	case "cladefsd_part_fetch_bytes_total/memory":
		dst = &f.MemoryBytes
	case "cladefsd_part_fetches_total/part_cache":
		dst = &f.PartCache
	case "cladefsd_part_fetch_bytes_total/part_cache":
		dst = &f.PartCacheBytes
	case "cladefsd_part_fetches_total/peer":
		dst = &f.Peer
	case "cladefsd_part_fetch_bytes_total/peer":
		dst = &f.PeerBytes
	case "cladefsd_part_fetches_total/bucket":
		dst = &f.Bucket
	case "cladefsd_part_fetch_bytes_total/bucket":
		dst = &f.BucketBytes
	case "cladefsd_tail_fetches_total/part_cache":
		dst = &f.TailsFromPartCache
	case "cladefsd_tail_fetches_total/bucket":
		dst = &f.TailsFromBucket
	default:
		slog.Warn("cladefsd fetch counter with an unknown source", "metric", name, "source", source)
		return
	}
	*dst = val
}

// partCacheShard reads one cladefsd_part_cache_shard_state sample: cladefsd
// reports 0=online, 1=suspect, 2=offline, 3=returning.
func partCacheShard(labels string, state uint64) *pb.PartCacheShard {
	shard, err := strconv.ParseUint(promLabel(labels, "shard"), 10, 32)
	if err != nil {
		slog.Warn("cladefsd part cache shard without a numeric shard label", "labels", labels, "err", err)
	}
	st := pb.PartCacheShardState_PART_CACHE_SHARD_STATE_UNSPECIFIED
	switch state {
	case 0:
		st = pb.PartCacheShardState_PART_CACHE_SHARD_STATE_ONLINE
	case 1:
		st = pb.PartCacheShardState_PART_CACHE_SHARD_STATE_SUSPECT
	case 2:
		st = pb.PartCacheShardState_PART_CACHE_SHARD_STATE_OFFLINE
	case 3:
		st = pb.PartCacheShardState_PART_CACHE_SHARD_STATE_RETURNING
	default:
		slog.Warn("cladefsd part cache shard in an unknown state", "labels", labels, "state", state)
	}
	return &pb.PartCacheShard{Shard: uint32(shard), Device: promLabel(labels, "device"), State: st}
}

// promLabel returns the value of label key in a raw Prometheus label block
// (`a="x",b="y"`), or "" if it is absent. cladefsd's label values never
// contain commas or escaped quotes.
func promLabel(labels, key string) string {
	for _, kv := range strings.Split(labels, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// parsePromLine splits a Prometheus text exposition sample line into its metric
// name, raw label block (without braces), and integer value.
func parsePromLine(line string) (name, labels string, val uint64, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", "", 0, false
	}
	var rest string
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.IndexByte(line[i+1:], '}')
		if j < 0 {
			return "", "", 0, false
		}
		name = line[:i]
		labels = line[i+1 : i+1+j]
		rest = strings.TrimSpace(line[i+1+j+1:])
	} else {
		var found bool
		name, rest, found = strings.Cut(line, " ")
		if !found {
			return "", "", 0, false
		}
		rest = strings.TrimSpace(rest)
	}
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	v, err := parsePromUint64(rest)
	if err != nil {
		return "", "", 0, false
	}
	return name, labels, v, true
}

func parsePromUint64(s string) (uint64, error) {
	if v, err := strconv.ParseUint(s, 10, 64); err == nil {
		return v, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid metric value %q", s)
	}
	return uint64(f), nil
}

func objectStoreStatsFromCounters(resp interface {
	GetCommitWaitNs() uint64
	GetPageInBytes() uint64
	GetFetchedBytes() uint64
	GetObjectStorage() *cladefsdv1.ObjectStorageStats
}) *pb.ObjectStoreStats {
	if resp == nil {
		return nil
	}
	out := &pb.ObjectStoreStats{
		CommitWaitNs: resp.GetCommitWaitNs(),
		PageInBytes:  resp.GetPageInBytes(),
		FetchedBytes: resp.GetFetchedBytes(),
	}
	if os := resp.GetObjectStorage(); os != nil {
		out.DownloadedBytes = os.GetDownloadedBytes()
		for k, v := range os.GetUploadedBytes() {
			out.UploadedBytes += v
			switch k {
			case "segments":
				out.UploadedSegmentsBytes += v
			case "extents":
				out.UploadedExtentsBytes += v
			case "checkpoints":
				out.UploadedCheckpointsBytes += v
			default:
				out.UploadedOtherBytes += v
			}
		}
		for k, v := range os.GetRequests() {
			switch k {
			case "get", "get-range":
				out.GetRequests += v
			case "create", "update", "overwrite":
				out.PutRequests += v
			default:
				out.OtherRequests += v
			}
		}
		for _, v := range os.GetFailures() {
			out.FailedRequests += v
		}
	}
	return out
}
