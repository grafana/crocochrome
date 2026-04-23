package chromium

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// HeapSnapshot is the aggregated result of a heap probe across all page targets in one chromium instance.
type HeapSnapshot struct {
	// V8HeapUsedBytes is the live object size on the V8 JS heap, summed across page renderers.
	V8HeapUsedBytes int64
	// V8HeapTotalBytes is the reserved size of the V8 JS heap, summed across page renderers.
	V8HeapTotalBytes int64
	// V8HeapLimitBytes is the V8-auto-detected JS heap ceiling (typically ~3.76GiB due to pointer compression).
	// Reported from a single page target — all renderers in the same chromium instance share this.
	V8HeapLimitBytes int64
	// V8EmbedderHeapBytes is the Oilpan (Blink C++) GC heap usage.
	V8EmbedderHeapBytes int64
	// V8BackingStoreBytes is the sum of ArrayBuffer and external string storage.
	V8BackingStoreBytes int64
	// DOMNodes is the total live DOM node count across page targets.
	DOMNodes int64
	// DOMDocuments is the total document count across page targets.
	DOMDocuments int64
	// DOMEventListeners is the total JS event listener count across page targets.
	DOMEventListeners int64
	// PageTargets is the number of page-type targets that contributed to this snapshot.
	// Zero means chromium had no pages open at probe time — the remaining fields are zero-valued and
	// callers should treat the snapshot as "not yet observable".
	PageTargets int
	// CollectedAt is the time the probe completed.
	CollectedAt time.Time
}

// HeapProber probes a chromium instance's V8 heap and DOM counters via CDP.
// Each Probe call dials a fresh websocket; the probe is stateless across calls.
type HeapProber struct {
	// BrowserWSURL is the chromium browser-level websocket URL, as reported by /json/version.
	BrowserWSURL string
}

// Probe connects to chromium, enumerates page targets, attaches to each in flatten mode,
// and gathers V8 heap + DOM counters. Values are summed across page targets.
// Returns a zero-PageTargets snapshot (with no error) when chromium has no page targets yet.
func (p *HeapProber) Probe(ctx context.Context) (HeapSnapshot, error) {
	conn, err := DialCDP(ctx, p.BrowserWSURL)
	if err != nil {
		return HeapSnapshot{}, fmt.Errorf("dialing chromium CDP: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := conn.Call(ctx, "", "Target.getTargets", nil, &targets); err != nil {
		return HeapSnapshot{}, fmt.Errorf("Target.getTargets: %w", err)
	}

	snap := HeapSnapshot{}

	for _, t := range targets.TargetInfos {
		if t.Type != "page" {
			continue
		}

		var attach struct {
			SessionID string `json:"sessionId"`
		}
		err := conn.Call(ctx, "", "Target.attachToTarget", map[string]any{
			"targetId": t.TargetID,
			"flatten":  true,
		}, &attach)
		if err != nil {
			return HeapSnapshot{}, fmt.Errorf("Target.attachToTarget(%s): %w", t.TargetID, err)
		}

		var heap struct {
			UsedSize             int64 `json:"usedSize"`
			TotalSize            int64 `json:"totalSize"`
			EmbedderHeapUsedSize int64 `json:"embedderHeapUsedSize"`
			BackingStoreSize     int64 `json:"backingStoreSize"`
		}
		if err := conn.Call(ctx, attach.SessionID, "Runtime.getHeapUsage", nil, &heap); err != nil {
			return HeapSnapshot{}, fmt.Errorf("Runtime.getHeapUsage: %w", err)
		}

		var dom struct {
			Documents        int64 `json:"documents"`
			Nodes            int64 `json:"nodes"`
			JSEventListeners int64 `json:"jsEventListeners"`
		}
		if err := conn.Call(ctx, attach.SessionID, "Memory.getDOMCounters", nil, &dom); err != nil {
			return HeapSnapshot{}, fmt.Errorf("Memory.getDOMCounters: %w", err)
		}

		// performance.memory.jsHeapSizeLimit is V8-auto-detected and identical across all renderers
		// in the same chromium instance. We only read it once (on the first page target).
		if snap.V8HeapLimitBytes == 0 {
			var evalOut struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			}
			err := conn.Call(ctx, attach.SessionID, "Runtime.evaluate", map[string]any{
				"expression":    `JSON.stringify({limit: performance.memory && performance.memory.jsHeapSizeLimit || 0})`,
				"returnByValue": true,
			}, &evalOut)
			if err != nil {
				return HeapSnapshot{}, fmt.Errorf("Runtime.evaluate(jsHeapSizeLimit): %w", err)
			}
			var parsed struct {
				Limit int64 `json:"limit"`
			}
			if err := json.Unmarshal([]byte(evalOut.Result.Value), &parsed); err == nil {
				snap.V8HeapLimitBytes = parsed.Limit
			}
		}

		snap.V8HeapUsedBytes += heap.UsedSize
		snap.V8HeapTotalBytes += heap.TotalSize
		snap.V8EmbedderHeapBytes += heap.EmbedderHeapUsedSize
		snap.V8BackingStoreBytes += heap.BackingStoreSize
		snap.DOMNodes += dom.Nodes
		snap.DOMDocuments += dom.Documents
		snap.DOMEventListeners += dom.JSEventListeners
		snap.PageTargets++
	}

	snap.CollectedAt = time.Now()
	return snap, nil
}
