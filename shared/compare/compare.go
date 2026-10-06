package compare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	Match     = "match"
	Different = "different"
	Error     = "error"
)

// SlowThreshold is the default latency rule: the candidate is flagged when it
// is this much slower than stable on the same request. 200ms is arbitrary but
// large enough that normal jitter on a laptop doesn't trigger it.
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

// Rules are the comparison settings. They exist because some fields are
// different on every request by design (a generated id, a timestamp), and
// calling those a regression would bury the real ones.
type Rules struct {
	// IgnoreFields are JSON fields that are expected to differ. A bare name
	// like "created_at" ignores that key wherever it appears; a dotted path
	// like "payment.reference" ignores only that one place.
	IgnoreFields []string
	// SlowThreshold is how much slower the candidate may be before the
	// comparison is flagged. Zero means use the package default.
	SlowThreshold time.Duration
}

func DefaultRules() Rules {
	return Rules{SlowThreshold: SlowThreshold}
}

// ParseRules reads the worker's two environment settings. Bad input is an
// error rather than a silent default, the same as the sampling config.
func ParseRules(ignoreFields, slowMs string) (Rules, error) {
	rules := DefaultRules()
	for _, f := range strings.Split(ignoreFields, ",") {
		if f = strings.TrimSpace(f); f != "" {
			rules.IgnoreFields = append(rules.IgnoreFields, f)
		}
	}
	if s := strings.TrimSpace(slowMs); s != "" {
		ms, err := strconv.Atoi(s)
		if err != nil || ms < 0 {
			return Rules{}, fmt.Errorf("COMPARE_SLOW_THRESHOLD_MS must be a number of milliseconds, got %q", slowMs)
		}
		rules.SlowThreshold = time.Duration(ms) * time.Millisecond
	}
	return rules, nil
}

// Compare uses the default rules. Handy for tests and for anything that
// doesn't have configuration to pass around.
func Compare(stable, candidate Response) Result {
	return DefaultRules().Compare(stable, candidate)
}

func (r Rules) Compare(stable, candidate Response) Result {
	// A candidate that never answered has no status or body to compare, so
	// the timeout or connection failure is the whole result.
	if candidate.Err != nil {
		return Result{Outcome: Error, Differences: []string{}}
	}

	threshold := r.SlowThreshold
	if threshold <= 0 {
		threshold = SlowThreshold
	}

	result := Result{
		StatusMatch:   stable.Status == candidate.Status,
		CandidateSlow: candidate.Latency-stable.Latency > threshold,
		Differences:   []string{},
	}
	bodyDiffs := r.DiffBodies(stable.Body, candidate.Body)
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

// DiffBodies compares two response bodies with the default rules.
func DiffBodies(stable, candidate []byte) []string {
	return DefaultRules().DiffBodies(stable, candidate)
}

// DiffBodies returns a list of readable differences between two response
// bodies. If both are valid JSON they are compared as parsed values, so key
// order and whitespace don't matter. Otherwise it falls back to the raw bytes.
func (r Rules) DiffBodies(stable, candidate []byte) []string {
	var a, b any
	if json.Unmarshal(stable, &a) != nil || json.Unmarshal(candidate, &b) != nil {
		if bytes.Equal(bytes.TrimSpace(stable), bytes.TrimSpace(candidate)) {
			return nil
		}
		return []string{"body: non-JSON bodies differ"}
	}

	d := differ{rules: r}
	d.walk("", a, b)
	return d.diffs
}

type differ struct {
	rules Rules
	diffs []string
}

// ignored reports whether a field is on the ignore list, either by bare name
// anywhere in the body or by its full dotted path.
func (r Rules) ignored(path, key string) bool {
	full := joinPath(path, key)
	for _, f := range r.IgnoreFields {
		if f == key || f == full {
			return true
		}
	}
	return false
}

func (d *differ) walk(path string, a, b any) {
	if len(d.diffs) >= maxDifferences {
		return
	}

	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			d.addDiff(path, a, b)
			return
		}
		for _, key := range unionKeys(av, bv) {
			// Checked per key as well as on entry, so one very wide object
			// can't write hundreds of differences into the database.
			if len(d.diffs) >= maxDifferences {
				return
			}
			x, inStable := av[key]
			y, inCandidate := bv[key]
			switch {
			case d.rules.ignored(path, key):
			case !inCandidate:
				d.diffs = append(d.diffs, joinPath(path, key)+": missing in candidate")
			case !inStable:
				d.diffs = append(d.diffs, joinPath(path, key)+": only in candidate")
			default:
				d.walk(joinPath(path, key), x, y)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			d.addDiff(path, a, b)
			return
		}
		if len(av) != len(bv) {
			d.diffs = append(d.diffs, fmt.Sprintf("%s: length %d != %d", displayPath(path), len(av), len(bv)))
		}
		for i := 0; i < min(len(av), len(bv)); i++ {
			d.walk(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i])
		}
	default:
		if !reflect.DeepEqual(a, b) {
			d.addDiff(path, a, b)
		}
	}
}

func (d *differ) addDiff(path string, a, b any) {
	d.diffs = append(d.diffs, fmt.Sprintf("%s: %s != %s", displayPath(path), shortJSON(a), shortJSON(b)))
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
