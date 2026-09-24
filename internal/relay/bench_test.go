package relay

import (
	"bytes"
	"testing"
)

const benchmarkContentChunk = `data: {"id":"chatcmpl-12345678","object":"chat.completion.chunk","created":1700000000,"model":"m","choices":[{"index":0,"delta":{"content":"hello world!"},"finish_reason":null}]}`
const benchmarkFinishChunk = `data: {"id":"chatcmpl-12345678","object":"chat.completion.chunk","created":1700000000,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

func BenchmarkPassthroughProcessLine(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		var out bytes.Buffer
		relay := NewPassthroughRelay(&out, estBody, "m", FormatChat, nil)
		if err := relay.ProcessLine(benchmarkContentChunk); err != nil {
			b.Fatal(err)
		}
		if err := relay.ProcessLine(benchmarkFinishChunk); err != nil {
			b.Fatal(err)
		}
		if err := relay.Flush(); err != nil {
			b.Fatal(err)
		}
		if out.Len() == 0 {
			b.Fatal("relay emitted no output")
		}
	}
}

func BenchmarkParseSSEToOpenAIResponse(b *testing.B) {
	raw := sseOf(benchmarkContentChunk, benchmarkFinishChunk)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		result, errBody, ok := ParseSSEToOpenAIResponse(raw, "m")
		if !ok || errBody != nil || result == nil {
			b.Fatal("fixture did not aggregate")
		}
	}
}

func BenchmarkFormatData(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		data := map[string]any{
			"id":     "response-benchmark",
			"object": "response",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 5,
				"perf_metrics":  nil,
			},
		}
		if frame := FormatData(data); len(frame) == 0 {
			b.Fatal("empty frame")
		}
	}
}
