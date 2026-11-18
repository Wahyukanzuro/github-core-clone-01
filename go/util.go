package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ---------- deterministic clock ----------
// Every timestamp in a response derives from MAKEFUN_TRIAL_NOW plus a
// monotonic counter (seconds). Never the wall clock.

var trialNowSec int64 = 1735689600
var trialNowStr string
var tsCounter int64

func initTrialClock() {
	raw := os.Getenv("MAKEFUN_TRIAL_NOW")
	if raw == "" {
		trialNowSec = 1735689600
	} else if v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
		// epoch seconds or epoch milliseconds
		if v > 4102444800 {
			trialNowSec = v / 1000
		} else {
			trialNowSec = v
		}
	} else if t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil {
		trialNowSec = t.Unix()
	}
	trialNowStr = time.Unix(trialNowSec, 0).UTC().Format(time.RFC3339)
}

// nextTime returns the next deterministic timestamp (RFC3339 UTC).
func nextTime() string {
	n := atomic.AddInt64(&tsCounter, 1)
	return time.Unix(trialNowSec+n, 0).UTC().Format(time.RFC3339)
}

// baseTime returns the trial start timestamp.
func baseTime() string { return trialNowStr }

// gitTime formats an epoch second the way git commit lines want it.
func gitTime(sec int64) string { return fmt.Sprintf("%d +0000", sec) }

// timeFormatRFC3339 formats an epoch second as RFC3339 UTC.
func timeFormatRFC3339(sec int64) string {
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// ---------- ids ----------

var reqCounter int64
var idCounter int64

// nextRequestID returns a deterministic request id.
func nextRequestID() string {
	n := atomic.AddInt64(&reqCounter, 1)
	return fmt.Sprintf("req-%08x", n)
}

// nextInternalID returns a deterministic increasing internal id.
func nextInternalID() int64 { return atomic.AddInt64(&idCounter, 1) }

// ---------- base64 ----------

func b64enc(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func b64dec(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// gid builds an opaque GraphQL global id.
func gid(kind string, parts ...interface{}) string {
	var sb strings.Builder
	sb.WriteString(kind)
	for _, p := range parts {
		sb.WriteByte(':')
		fmt.Fprint(&sb, p)
	}
	return b64enc([]byte(sb.String()))
}

// parseGID decodes a global id into kind + fields.
func parseGID(s string) (string, []string, bool) {
	b, err := b64dec(s)
	if err != nil {
		return "", nil, false
	}
	parts := strings.Split(string(b), ":")
	if len(parts) < 2 {
		return "", nil, false
	}
	return parts[0], parts[1:], true
}

// ---------- sha1 ----------

func sha1Hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}

// ---------- json ----------

// J is a JSON object (ordered by key on marshal via encoding/json map sort).
type J map[string]interface{}

func jsonBytes(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// jsonUnmarshal decodes JSON into v.
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

func jsonString(v interface{}) string { return string(jsonBytes(v)) }

// parseJSONBody decodes a JSON object body; returns ok=false on malformed JSON.
func parseJSONBody(body []byte) (map[string]interface{}, bool) {
	if len(body) == 0 {
		return map[string]interface{}{}, true
	}
	var m map[string]interface{}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&m); err != nil {
		return nil, false
	}
	if m == nil {
		m = map[string]interface{}{}
	}
	return m, true
}

func strField(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolField(m map[string]interface{}, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func intField(m map[string]interface{}, key string, def int) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return def
}

func strSliceField(m map[string]interface{}, key string) []string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// sortedKeys returns the keys of a string map in sorted order.
func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// lineCount counts lines in content (>=1 for non-empty).
func lineCount(s string) int {
	if len(s) == 0 {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
