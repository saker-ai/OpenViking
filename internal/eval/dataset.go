package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// LoadDataset reads a JSONL dataset from path. Each non-empty line is
// decoded as an EvalCase. Lines that fail JSON decoding are skipped
// with a warning written to logOut (or stderr when logOut is nil); the
// function returns the valid cases and a nil error so a single bad
// line does not abort the whole load. Mirrors the Python
// rag_eval.load_questions behavior.
//
// The JSONL shape matches openviking/eval/datasets/*.jsonl:
//
//	{"question": "...", "answer": "...", "files": ["..."], ...}
//
// We accept both "question" and "query" keys for the question field,
// and both "answer" and "ground_truth" keys for the reference answer.
// "id" is optional (the runner assigns a stable one when absent).
func LoadDataset(path string, logOut io.Writer) ([]EvalCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("eval: open dataset %s: %w", path, err)
	}
	defer f.Close()
	return LoadDatasetFromReader(f, logOut)
}

// LoadDatasetFromReader is like LoadDataset but reads from r. Used by
// tests that have an in-memory JSONL string.
func LoadDatasetFromReader(r io.Reader, logOut io.Writer) ([]EvalCase, error) {
	if logOut == nil {
		logOut = io.Discard
	}
	dec := bufio.NewReader(r)
	var cases []EvalCase
	lineNum := 0
	for {
		line, err := dec.ReadString('\n')
		if line != "" {
			lineNum++
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				// Continue reading even if this line was blank.
				if err != nil {
					break
				}
				continue
			}
			c, lerr := decodeCase(trimmed)
			if lerr != nil {
				fmt.Fprintf(logOut, "eval: line %d: %v\n", lineNum, lerr)
			} else if c.Query == "" {
				fmt.Fprintf(logOut, "eval: line %d: missing 'question' field\n", lineNum)
			} else {
				cases = append(cases, c)
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			if err != io.EOF {
				return cases, fmt.Errorf("eval: read dataset: %w", err)
			}
		}
	}
	return cases, nil
}

// decodeCase parses one JSONL line into an EvalCase, accepting both
// the Python (question/answer) and Go (query/ground_truth) key names.
func decodeCase(line string) (EvalCase, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return EvalCase{}, fmt.Errorf("invalid JSON: %w", err)
	}
	c := EvalCase{}
	if v, ok := raw["id"].(string); ok {
		c.ID = v
	}
	// Question: prefer "query" (Go), fall back to "question" (Python).
	if v, ok := raw["query"].(string); ok && v != "" {
		c.Query = v
	} else if v, ok := raw["question"].(string); ok {
		c.Query = v
	}
	// GroundTruth: prefer "ground_truth" (Go), fall back to "answer".
	if v, ok := raw["ground_truth"].(string); ok && v != "" {
		c.GroundTruth = v
	} else if v, ok := raw["answer"].(string); ok {
		c.GroundTruth = v
	}
	// Meta: pull a few well-known keys for traceability.
	meta := map[string]string{}
	if files, ok := raw["files"].([]any); ok {
		var parts []string
		for _, f := range files {
			if s, ok := f.(string); ok {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			meta["files"] = strings.Join(parts, ",")
		}
	}
	if v, ok := raw["category"].(string); ok {
		meta["category"] = v
	}
	if len(meta) > 0 {
		c.Meta = meta
	}
	return c, nil
}
