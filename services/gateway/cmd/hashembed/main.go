// Command hashembed prints the deterministic hash embedding of each line of stdin as JSON.
//
// It exists so the Python embedder sidecar and the Go mock provider can be proven to agree.
// The two implementations are independent translations of the same construction, and a silent
// divergence between them would mean the calibrated cache threshold no longer described the
// cache that is actually running -- a bug that would show up as a mysteriously bad hit rate
// rather than as a failure.
//
//	go run ./cmd/hashembed < prompts.txt > golden.json
//
// The output is consumed by benchmarks/tests/test_hash_embed_parity.py.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/providers/mock"
)

type record struct {
	Text   string    `json:"text"`
	Vector []float32 `json:"vector"`
}

func main() {
	dim := flag.Int("dim", 384, "embedding dimension")
	flag.Parse()

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	out := make([]record, 0, 64)
	for sc.Scan() {
		text := sc.Text()
		if text == "" {
			continue
		}
		out = append(out, record{Text: text, Vector: mock.HashEmbed(text, *dim)})
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "reading stdin:", err)
		os.Exit(1)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "encoding:", err)
		os.Exit(1)
	}
}
