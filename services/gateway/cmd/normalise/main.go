// Command normalise prints the cache exact-tier key for each line of stdin as JSON.
//
// It exists so the Go cache and the Python calibration harness can be proven to agree on what
// counts as "the same question". They are independent implementations of the same rule, and a
// divergence would mean the calibrated hit rate does not describe the running cache.
//
//	go run ./cmd/normalise < prompts.txt > golden.json
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/cache"
)

type record struct {
	Text string `json:"text"`
	Key  string `json:"key"`
}

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	out := make([]record, 0, 64)
	for sc.Scan() {
		text := sc.Text()
		if text == "" {
			continue
		}
		out = append(out, record{Text: text, Key: cache.ExactKey(text)})
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
