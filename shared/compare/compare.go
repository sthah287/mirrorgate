package compare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"
)

const (
	Match     = "match"
	Different = "different"
	Error     = "error"
)

// SlowThreshold is how much slower the candidate can be before the
// comparison gets flagged. 200ms is arbitrary but large enough that normal
// jitter on a laptop doesn't trigger it.
const SlowThreshold = 200 * time.Millisecond

const maxDifferences = 20

type Response struct {
	Status  int
	Body    []byte
	Latency time.Duration
	Err     error
}

type Result struct {
	Outcome       string
	StatusMatch   bool
	BodyMatch     bool
	CandidateSlow bool
	Differences   []string
}

func Compare(stable, candidate Response) Result {
	if candidate.Err != nil {
		return Result{Outcome: Error, Differences: []string{}}
	}

	result := Result{
		StatusMatch:   stable.Status == candidate.Status,
		CandidateSlow: candidate.Latency-stable.Latency > SlowThreshold,
		Differences:   []string{},
	}
	bodyDiffs := DiffBodies(stable.Body, candidate.Body)
	result.BodyMatch = len(bodyDiffs) == 0

	// When the status is different the bodies are usually completely
	// different too (a product vs an error message), and listing every field
	// just buries the status change.
	if !result.StatusMatch {
		result.Differences = append(result.Differences,
			fmt.Sprintf("status: %d != %d", stable.Status, candidate.Status))
		if !result.BodyMatch {
			result.Differences = append(result.Differences, "body: differs")
		}
	} else {
		result.Differences = append(result.Differences, bodyDiffs...)
	}

	switch {
	case candidate.Status >= 500 && stable.Status < 500:
		result.Outcome = Error
	case !result.StatusMatch || !result.BodyMatch:
		result.Outcome = Different
	default:
		result.Outcome = Match
	}
	return result
}

// DiffBodies returns a list of human readable differences between two
// response bodies. If both bodies are valid JSON they are compared as parsed
// values, so key order and whitespace don't matter. Otherwise it falls back
// to comparing the raw bytes.
func DiffBodies(stable, candidate []byte) []string {
	var a, b any
	if json.Unmarshal(stable, &a) != nil || json.Unmarshal(candidate, &b) != nil {
		if bytes.Equal(bytes.TrimSpace(stable), bytes.TrimSpace(candidate)) {
			return nil
		}
		return []string{"body: non-JSON bodies differ"}
	}

	var diffs []string
	diffJSON("", a, b, &diffs)
	return diffs
}

func diffJSON(path string, a, b any, diffs *[]string) {
	if len(*diffs) >= maxDifferences {
		return
	}

	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			addDiff(diffs, path, a, b)
			return
		}
		for _, key := range unionKeys(av, bv) {
			x, inStable := av[key]
			y, inCandidate := bv[key]
			switch {
			case !inCandidate:
				*diffs = append(*diffs, joinPath(path, key)+": missing in candidate")
			case !inStable:
				*diffs = append(*diffs, joinPath(path, key)+": only in candidate")
			default:
				diffJSON(joinPath(path, key), x, y, diffs)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			addDiff(diffs, path, a, b)
			return
		}
		if len(av) != len(bv) {
			*diffs = append(*diffs, fmt.Sprintf("%s: length %d != %d", displayPath(path), len(av), len(bv)))
		}
		for i := 0; i < min(len(av), len(bv)); i++ {
			diffJSON(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i], diffs)
		}
	default:
		if !reflect.DeepEqual(a, b) {
			addDiff(diffs, path, a, b)
		}
	}
}

func addDiff(diffs *[]string, path string, a, b any) {
	*diffs = append(*diffs, fmt.Sprintf("%s: %s != %s", displayPath(path), shortJSON(a), shortJSON(b)))
}

func unionKeys(a, b map[string]any) []string {
	seen := make(map[string]bool, len(a))
	keys := make([]string, 0, len(a))
	for k := range a {
		seen[k] = true
		keys = append(keys, k)
	}
	for k := range b {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func displayPath(path string) string {
	if path == "" {
		return "body"
	}
	return path
}

func shortJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	if len(b) > 60 {
		return string(b[:57]) + "..."
	}
	return string(b)
}
