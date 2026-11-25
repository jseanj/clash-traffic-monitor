package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Subscription values come from FlClash's provider metadata, never from the
// monitor's partial connection history. Subscription URLs and secrets stay local.
type flClashSubscription struct {
	Configured bool   `json:"configured"`
	Name       string `json:"name"`
	Upload     int64  `json:"upload"`
	Download   int64  `json:"download"`
	Total      int64  `json:"total"`
	Used       int64  `json:"used"`
	Remaining  int64  `json:"remaining"`
	Expire     int64  `json:"expire"`
	UpdatedAt  int64  `json:"updatedAt"`
}

func readFlClashSubscription(ctx context.Context, path string, profileID int64) (flClashSubscription, error) {
	var result flClashSubscription
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_busy_timeout", "1500")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return result, err
	}
	defer db.Close()
	var info sql.NullString
	var updated sql.NullInt64
	err = db.QueryRowContext(ctx, "SELECT label, subscription_info, last_update_date FROM profiles WHERE id = ?", profileID).Scan(&result.Name, &info, &updated)
	if err != nil {
		return result, err
	}
	if !info.Valid || info.String == "" || info.String == "null" {
		return result, errors.New("subscription metadata unavailable")
	}
	if err := json.Unmarshal([]byte(info.String), &result); err != nil {
		return result, err
	}
	if result.Upload < 0 || result.Download < 0 || result.Total < 0 || result.Expire < 0 || result.Upload > math.MaxInt64-result.Download {
		return result, errors.New("invalid subscription counters")
	}
	result.Configured = true
	result.UpdatedAt = updated.Int64
	result.Used = result.Upload + result.Download
	result.Remaining = max(int64(0), result.Total-result.Used)
	if result.Name == "" || result.Name == strconv.FormatInt(profileID, 10) {
		result.Name = "当前订阅"
	}
	return result, nil
}

func currentFlClashProfile(ctx context.Context) (int64, error) {
	if value := os.Getenv("TRAFFIC_MONITOR_FLCLASH_PROFILE_ID"); value != "" {
		return strconv.ParseInt(value, 10, 64)
	}
	// defaults reads the live macOS preferences domain, including a selection
	// switched by FlClash since the monitor was started.
	out, err := exec.CommandContext(ctx, "/usr/bin/defaults", "read", "com.follow.clash", "flutter.config").Output()
	if err != nil {
		return 0, err
	}
	var cfg struct {
		CurrentProfileID json.Number `json:"currentProfileId"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return 0, err
	}
	return cfg.CurrentProfileID.Int64()
}

func (s *service) handleFlClashSubscription(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	path := strings.TrimSpace(os.Getenv("TRAFFIC_MONITOR_FLCLASH_DB"))
	if path == "" {
		writeJSON(w, http.StatusOK, flClashSubscription{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	id, err := currentFlClashProfile(ctx)
	if err == nil {
		var data flClashSubscription
		data, err = readFlClashSubscription(ctx, path, id)
		if err == nil {
			writeJSON(w, http.StatusOK, data)
			return
		}
	}
	// Do not expose local paths, subscription links, or raw metadata on errors.
	writeError(w, http.StatusServiceUnavailable, fmt.Errorf("无法读取当前订阅额度，请在 FlClash 中同步订阅后重试"))
}
