package leader

// The engine's side of a prefill/decode handoff on /metrics (PLAN T28.13,
// ADR-122). The router times each handoff per pair itself
// (opod_pd_handoff_seconds, internal/metrics); what only the engine knows —
// the bytes its KV connector moved, how long the transfers took, and how many
// requests wait on a KV load — arrives in each worker's heartbeat sample
// (nodeapi.EngineLoad) and is re-exported here per worker, at scrape time,
// from the samples the leader already holds.
//
// Unknown is absent: a worker whose engine does not state a figure (no KV
// connector, an engine that publishes none) has no series for it, never a 0.
// The engine labels its transfers by engine, not by peer, so these are per
// worker — the node that moved the KV — and a pair's figure is the router's
// histogram. The series are bounded by the leader's workers: a forgotten
// node's sample leaves nodeLoad (forgetNode), and a stale one is skipped.

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type pdTransferCollector struct {
	s *Server

	bytes, seconds, count, fetch *prometheus.Desc
}

func newPDTransferCollector(s *Server) *pdTransferCollector {
	node := []string{"node"}
	return &pdTransferCollector{
		s: s,
		bytes: prometheus.NewDesc("opod_pd_transfer_bytes_total",
			"KV bytes this worker's engine moved for prefill/decode handoffs, since the engine started (vLLM: nixl_bytes_transferred_sum), by worker node id. Absent when the engine does not say.", node, nil),
		seconds: prometheus.NewDesc("opod_pd_transfer_seconds_total",
			"Seconds this worker's engine spent in KV transfers, summed, since the engine started (vLLM: nixl_xfer_time_seconds_sum). rate(bytes) / rate(seconds) is its transfer speed. Absent when the engine does not say.", node, nil),
		count: prometheus.NewDesc("opod_pd_transfers_total",
			"KV transfers this worker's engine completed, since the engine started (vLLM: nixl_xfer_time_seconds_count). Absent when the engine does not say.", node, nil),
		fetch: prometheus.NewDesc("opod_pd_kv_fetch_requests",
			"Requests on this worker waiting on a remote KV load right now, by stage: waiting_to_start, in_progress (the transfer), loaded (KV in place, waiting for the decoder) (vLLM: num_requests_kv_fetch_by_stage). Absent when the engine does not say.", []string{"node", "stage"}, nil),
	}
}

func (c *pdTransferCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.bytes
	ch <- c.seconds
	ch <- c.count
	ch <- c.fetch
}

func (c *pdTransferCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	c.s.nodeLoad.Range(func(k, v any) bool {
		node, _ := k.(string)
		ld, ok := v.(nodeLoadSample)
		if !ok || node == "" || now.Sub(ld.at) > loadSampleMaxAge {
			return true
		}
		if p := ld.KVXferBytesTotal; p != nil {
			ch <- prometheus.MustNewConstMetric(c.bytes, prometheus.CounterValue, float64(*p), node)
		}
		if p := ld.KVXferSecondsTotal; p != nil {
			ch <- prometheus.MustNewConstMetric(c.seconds, prometheus.CounterValue, *p, node)
		}
		if p := ld.KVXfersTotal; p != nil {
			ch <- prometheus.MustNewConstMetric(c.count, prometheus.CounterValue, float64(*p), node)
		}
		for stage, p := range map[string]*int64{
			"waiting_to_start": ld.KVFetchWaitingToStart,
			"in_progress":      ld.KVFetchInProgress,
			"loaded":           ld.KVFetchLoaded,
		} {
			if p != nil {
				ch <- prometheus.MustNewConstMetric(c.fetch, prometheus.GaugeValue, float64(*p), node, stage)
			}
		}
		return true
	})
}
