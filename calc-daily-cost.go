// calc-daily-cost.go — statusline backend used by show-statusline.sh.
//
// Build:
//
//	go build -o calc-daily-cost ./calc-daily-cost.go
//
// Subcommands:
//
//	calc-daily-cost daily-cost   (default) Scans ~/.claude/projects/**/*.jsonl with
//	                             per-file incremental rescan and prints
//	                             {"daily_cost": ..., "burn_rate_cost_per_hour": ...}.
//	calc-daily-cost rate-limits  Reads a statusline-input JSON from stdin and prints
//	                             a single line summarising rate_limits.{five_hour,seven_day}
//	                             as colored progress bars with JST reset times.
//
// State files (auto-created by daily-cost):
//
//	/tmp/cc-daily-cost-cache-<uid>.json   — 60 s output cache
//	/tmp/cc-daily-cost-state-<uid>.json   — per-file (mtime, size, daily breakdown, recent entries)
//
// Update PRICING with values from https://docs.anthropic.com/en/docs/about-claude/pricing when Anthropic changes its rates.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	cacheTTL          = 60 * time.Second
	recentWindow      = 6 * time.Hour
	burnRateBlock     = 5 * time.Hour
	maxScannerLineLen = 4 * 1024 * 1024
)

type priceRow struct{ Input, Output, CacheCreate, CacheRead float64 }

var pricing = map[string]priceRow{
	"claude-opus-4-7":           {15e-6, 75e-6, 18.75e-6, 1.5e-6},
	"claude-opus-4-8":           {15e-6, 75e-6, 18.75e-6, 1.5e-6},
	"claude-sonnet-4-6":         {3e-6, 15e-6, 3.75e-6, 0.3e-6},
	"claude-haiku-4-5-20251001": {1e-6, 5e-6, 1.25e-6, 0.1e-6},
	"claude-fable-5":            {3e-6, 15e-6, 3.75e-6, 0.3e-6},
}

var modelAliases = map[string]string{
	"claude-opus-4-7":            "claude-opus-4-7",
	"claude-opus-4-7-20250619":   "claude-opus-4-7",
	"claude-opus-4-8":            "claude-opus-4-8",
	"claude-opus-4-8-20250619":   "claude-opus-4-8",
	"claude-sonnet-4-6":          "claude-sonnet-4-6",
	"claude-sonnet-4-6-20250514": "claude-sonnet-4-6",
	"claude-haiku-4-5":           "claude-haiku-4-5-20251001",
	"claude-haiku-4-5-20251001":  "claude-haiku-4-5-20251001",
	"claude-fable-5":             "claude-fable-5",
}

var jst = time.FixedZone("JST", 9*3600)

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type assistantMsg struct {
	Usage *usage `json:"usage"`
	Model string `json:"model"`
}

type record struct {
	Type      string        `json:"type"`
	Timestamp string        `json:"timestamp"`
	Message   *assistantMsg `json:"message"`
}

type recentEntry struct {
	TSUnix int64   `json:"t"`
	Cost   float64 `json:"c"`
}

type fileState struct {
	Mtime         int64              `json:"mtime"`
	Size          int64              `json:"size"`
	DailyCosts    map[string]float64 `json:"daily"`
	RecentEntries []recentEntry      `json:"recent"`
}

type stateFile struct {
	Files map[string]*fileState `json:"files"`
}

func entryCost(u *usage, model string) float64 {
	p, ok := pricing[modelAliases[model]]
	if !ok {
		return 0
	}
	return float64(u.InputTokens)*p.Input +
		float64(u.OutputTokens)*p.Output +
		float64(u.CacheCreationInputTokens)*p.CacheCreate +
		float64(u.CacheReadInputTokens)*p.CacheRead
}

func parseTimestamp(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// parseFrom reads JSONL records from f starting at startOffset and folds them into daily/recent.
func parseFrom(f *os.File, startOffset int64, daily map[string]float64, recent *[]recentEntry, recentCutoff time.Time) error {
	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxScannerLineLen)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r record
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if r.Type != "assistant" || r.Message == nil || r.Message.Usage == nil || r.Timestamp == "" {
			continue
		}
		ts, ok := parseTimestamp(r.Timestamp)
		if !ok {
			continue
		}
		c := entryCost(r.Message.Usage, r.Message.Model)
		dateKey := ts.In(jst).Format("2006-01-02")
		daily[dateKey] += c
		if !ts.Before(recentCutoff) {
			*recent = append(*recent, recentEntry{TSUnix: ts.Unix(), Cost: c})
		}
	}
	return sc.Err()
}

func loadState(path string) *stateFile {
	st := &stateFile{Files: map[string]*fileState{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, st)
	if st.Files == nil {
		st.Files = map[string]*fileState{}
	}
	return st
}

func saveState(path string, st *stateFile) {
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
}

func discoverFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var paths []string
	for _, base := range []string{filepath.Join(home, ".claude"), filepath.Join(home, ".config", "claude")} {
		root := filepath.Join(base, "projects")
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if strings.HasSuffix(p, ".jsonl") {
				paths = append(paths, p)
			}
			return nil
		})
	}
	return paths
}

func rescan(path string, prev *fileState, recentCutoff time.Time) (*fileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	mtime := info.ModTime().Unix()
	size := info.Size()
	// Unchanged → reuse, but drop recent entries that fell out of the window.
	if prev != nil && prev.Mtime == mtime && prev.Size == size {
		trimmed := prev.RecentEntries[:0]
		for _, e := range prev.RecentEntries {
			if time.Unix(e.TSUnix, 0).After(recentCutoff) {
				trimmed = append(trimmed, e)
			}
		}
		prev.RecentEntries = trimmed
		return prev, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Decide whether we can append-extend or must full-rescan.
	startOffset := int64(0)
	daily := map[string]float64{}
	var recent []recentEntry
	if prev != nil && size >= prev.Size {
		// Trust that JSONL is append-only when size grew without size shrink.
		startOffset = prev.Size
		for k, v := range prev.DailyCosts {
			daily[k] = v
		}
		for _, e := range prev.RecentEntries {
			if time.Unix(e.TSUnix, 0).After(recentCutoff) {
				recent = append(recent, e)
			}
		}
	}
	if err := parseFrom(f, startOffset, daily, &recent, recentCutoff); err != nil {
		return nil, err
	}
	return &fileState{Mtime: mtime, Size: size, DailyCosts: daily, RecentEntries: recent}, nil
}

func calcBurnRate(all []recentEntry, now time.Time) (float64, bool) {
	if len(all) < 2 {
		return 0, false
	}
	sort.Slice(all, func(i, j int) bool { return all[i].TSUnix < all[j].TSUnix })

	var blockStart time.Time
	var block []recentEntry
	for _, e := range all {
		ts := time.Unix(e.TSUnix, 0)
		if blockStart.IsZero() {
			blockStart = ts.Truncate(time.Hour)
			block = []recentEntry{e}
			continue
		}
		last := time.Unix(block[len(block)-1].TSUnix, 0)
		if ts.Sub(blockStart) > burnRateBlock || ts.Sub(last) > burnRateBlock {
			blockStart = ts.Truncate(time.Hour)
			block = []recentEntry{e}
		} else {
			block = append(block, e)
		}
	}
	if len(block) < 2 {
		return 0, false
	}
	if now.Sub(time.Unix(block[len(block)-1].TSUnix, 0)) >= burnRateBlock {
		return 0, false
	}
	durMin := float64(block[len(block)-1].TSUnix-block[0].TSUnix) / 60.0
	if durMin <= 0 {
		return 0, false
	}
	var total float64
	for _, e := range block {
		total += e.Cost
	}
	if durMin >= 60 {
		return (total / durMin) * 60, true
	}
	return total, true
}

func writeCache(path, content string) {
	_ = os.WriteFile(path, []byte(content), 0o600)
}

func main() {
	cmd := "daily-cost"
	if len(os.Args) >= 2 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "daily-cost", "":
		runDailyCost()
	case "rate-limits":
		runRateLimits()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", cmd)
		os.Exit(2)
	}
}

func runDailyCost() {
	uid := os.Getuid()
	cachePath := fmt.Sprintf("/tmp/cc-daily-cost-cache-%d.json", uid)
	statePath := fmt.Sprintf("/tmp/cc-daily-cost-state-%d.json", uid)

	if info, err := os.Stat(cachePath); err == nil && time.Since(info.ModTime()) < cacheTTL {
		if b, err := os.ReadFile(cachePath); err == nil {
			os.Stdout.Write(b)
			fmt.Println()
			return
		}
	}

	state := loadState(statePath)
	files := discoverFiles()
	now := time.Now().UTC()
	recentCutoff := now.Add(-recentWindow)

	newFiles := make(map[string]*fileState, len(files))
	totalDaily := map[string]float64{}
	var allRecent []recentEntry

	for _, p := range files {
		prev := state.Files[p]
		fs, err := rescan(p, prev, recentCutoff)
		if err != nil || fs == nil {
			continue
		}
		newFiles[p] = fs
		for k, v := range fs.DailyCosts {
			totalDaily[k] += v
		}
		allRecent = append(allRecent, fs.RecentEntries...)
	}

	today := time.Now().In(jst).Format("2006-01-02")
	out := map[string]any{"daily_cost": roundTo(totalDaily[today], 2)}
	if br, ok := calcBurnRate(allRecent, now); ok {
		out["burn_rate_cost_per_hour"] = roundTo(br, 2)
	}

	b, _ := json.Marshal(out)
	fmt.Println(string(b))

	writeCache(cachePath, string(b))
	saveState(statePath, &stateFile{Files: newFiles})
}

type rateLimitInfo struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       any      `json:"resets_at"`
}

type rateLimitsInput struct {
	RateLimits struct {
		FiveHour *rateLimitInfo `json:"five_hour"`
		SevenDay *rateLimitInfo `json:"seven_day"`
	} `json:"rate_limits"`
}

// time.Weekday: Sunday=0..Saturday=6.
var japaneseWeekday = []rune("日月火水木金土")

func parseResetsAt(v any) (time.Time, bool) {
	switch x := v.(type) {
	case float64:
		return time.Unix(int64(x), 0), true
	case string:
		s := strings.Replace(x, "Z", "+00:00", 1)
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, true
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func runRateLimits() {
	var in rateLimitsInput
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		return
	}
	type slot struct {
		label  string
		info   *rateLimitInfo
		isWeek bool
	}
	slots := []slot{
		{"5h", in.RateLimits.FiveHour, false},
		{"7d", in.RateLimits.SevenDay, true},
	}
	var parts []string
	for _, s := range slots {
		if s.info == nil || s.info.UsedPercentage == nil {
			continue
		}
		u := int(math.Round(*s.info.UsedPercentage))
		if u < 0 {
			u = 0
		}
		if u > 100 {
			u = 100
		}
		filled := u / 10
		bar := strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)
		color := "\033[32m"
		if u >= 70 {
			color = "\033[31m"
		} else if u >= 40 {
			color = "\033[33m"
		}
		resetStr := ""
		if s.info.ResetsAt != nil {
			if ts, ok := parseResetsAt(s.info.ResetsAt); ok {
				dt := ts.In(jst)
				if s.isWeek {
					wd := japaneseWeekday[int(dt.Weekday())]
					resetStr = fmt.Sprintf(" ~%d/%d(%s)%d:%02d", int(dt.Month()), dt.Day(), string(wd), dt.Hour(), dt.Minute())
				} else {
					resetStr = fmt.Sprintf(" ~%d:%02d", dt.Hour(), dt.Minute())
				}
			}
		}
		parts = append(parts, fmt.Sprintf("%s %s%s\033[0m %d%%%s", s.label, color, bar, u, resetStr))
	}
	if len(parts) > 0 {
		fmt.Println(strings.Join(parts, " | "))
	}
}

func roundTo(v float64, places int) float64 {
	shift := 1.0
	for i := 0; i < places; i++ {
		shift *= 10
	}
	return float64(int64(v*shift+0.5)) / shift
}
