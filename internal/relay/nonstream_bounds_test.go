package relay

// Bounds tests for the Responses SSE→JSON aggregation's output_index
// handling. The dense placeholder fill (streamToJsonConverter.js:89-93) is
// load-bearing for real streams — interior gaps keep their slots — but it
// must never allocate super-linearly for an upstream-controlled index, so
// processAggMessage drops events beyond config.MaxResponsesOutputIndex
// before they reach the items map.

import (
	"runtime"
	"testing"
	"time"

	"opencode-free-proxy/internal/config"
)

func outputItemEvent(idx int64, text string) string {
	return outputItemEventRaw(itoa(idx), text)
}

func outputItemEventRaw(idxLiteral, text string) string {
	return "event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":` +
		idxLiteral + `,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}` + "\n\n"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	var buf [21]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestConvertResponsesStreamToJsonHostileOutputIndex: one event claiming
// output_index 1e18 must not drive the dense fill — before the cap this
// allocated 1e18 placeholder maps and OOM'd the process. The deadline guard
// is the assertion (allocation counts are too GC-dependent to be stable); a
// hostile index completes in bounded time with the offending item dropped.
func TestConvertResponsesStreamToJsonHostileOutputIndex(t *testing.T) {
	cases := []struct {
		name       string
		idxLiteral string
	}{
		{"1e18", "1000000000000000000"},
		{"max float64", "1.7976931348623157e308"},
		{"just past the cap", itoa(int64(config.MaxResponsesOutputIndex) + 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := outputItemEventRaw(tc.idxLiteral, "boom")
			done := make(chan map[string]any, 1)
			go func() {
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				got := ConvertResponsesStreamToJson(raw)
				runtime.ReadMemStats(&after)
				if mallocs := after.Mallocs - before.Mallocs; mallocs > 1_000_000 {
					t.Errorf("hostile output_index allocated %d objects — the dense fill is not bounded", mallocs)
				}
				done <- got
			}()
			select {
			case got := <-done:
				if out, ok := got["output"].([]any); ok && len(out) != 0 {
					t.Fatalf("output = %d slots, want 0 (the hostile item is dropped)", len(out))
				}
			case <-time.After(3 * time.Second):
				t.Fatal("hostile output_index hung the aggregation (unbounded placeholder fill)")
			}
		})
	}
}

// TestConvertResponsesStreamToJsonSaneGapsStillFill pins the ORIGINAL
// placeholder semantics for sane indexes: items at 0 and 3 leave exact
// placeholder slots at 1 and 2 (streamToJsonConverter.js:92
// `state.items.get(i) || { type: "message", content: [], role: "assistant" }`).
func TestConvertResponsesStreamToJsonSaneGapsStillFill(t *testing.T) {
	raw := outputItemEvent(0, "first") + outputItemEvent(3, "last")
	got := ConvertResponsesStreamToJson(raw)
	out, ok := got["output"].([]any)
	if !ok || len(out) != 4 {
		t.Fatalf("output = %#v, want 4 slots (items at 0 and 3, placeholders at 1 and 2)", got["output"])
	}
	wantHole := map[string]any{"type": "message", "content": []any{}, "role": "assistant"}
	for _, hole := range []int{1, 2} {
		if got := out[hole]; !equalMap(got, wantHole) {
			t.Fatalf("slot %d = %v, want the placeholder %v", hole, got, wantHole)
		}
	}
	for slot, want := range map[int]string{0: "first", 3: "last"} {
		item, _ := out[slot].(map[string]any)
		if item == nil || item["type"] != "message" {
			t.Fatalf("slot %d = %v, want the stored item", slot, out[slot])
		}
		content, _ := item["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("slot %d content = %v, want one output_text block", slot, content)
		}
		block, _ := content[0].(map[string]any)
		if block["text"] != want {
			t.Fatalf("slot %d text = %v, want %q", slot, block["text"], want)
		}
	}
}

// TestConvertResponsesStreamToJsonCapBoundary: an item exactly AT the cap is
// still accepted (the bound is inclusive — real streams are nowhere near it),
// its fill stays bounded, and a beyond-cap sibling next to a sane one drops
// only the hostile event.
func TestConvertResponsesStreamToJsonCapBoundary(t *testing.T) {
	t.Run("index at the cap fills bounded placeholders", func(t *testing.T) {
		raw := outputItemEvent(int64(config.MaxResponsesOutputIndex), "edge")
		got := ConvertResponsesStreamToJson(raw)
		out, ok := got["output"].([]any)
		if !ok || len(out) != config.MaxResponsesOutputIndex+1 {
			t.Fatalf("output slots = %d, want %d", len(out), config.MaxResponsesOutputIndex+1)
		}
	})
	t.Run("beyond-cap event dropped, sane sibling kept", func(t *testing.T) {
		raw := outputItemEvent(1, "kept") + outputItemEvent(int64(config.MaxResponsesOutputIndex)+1, "dropped")
		got := ConvertResponsesStreamToJson(raw)
		out := got["output"].([]any)
		if len(out) != 2 {
			t.Fatalf("output = %d slots, want 2 (index-0 placeholder + the kept item)", len(out))
		}
	})
}

func equalMap(got, want any) bool {
	g, ok := got.(map[string]any)
	if !ok {
		return false
	}
	w, ok := want.(map[string]any)
	if !ok || len(g) != len(w) {
		return false
	}
	for k, v := range w {
		gv, has := g[k]
		if !has {
			return false
		}
		switch v := v.(type) {
		case []any:
			ga, is := gv.([]any)
			if !is || len(ga) != len(v) {
				return false
			}
		default:
			if gv != v {
				return false
			}
		}
	}
	return true
}
