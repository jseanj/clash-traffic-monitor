package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFlClashSubscriptionHandler(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flclash.sqlite")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE profiles (id INTEGER PRIMARY KEY, label TEXT, subscription_info TEXT, last_update_date INTEGER);
	INSERT INTO profiles VALUES(1, '1', '{"upload":20,"download":30,"total":100,"expire":1900000000}', 1800000000);
	INSERT INTO profiles VALUES(2, 'Second', '{"upload":80,"download":40,"total":100}', 1800000010);
	INSERT INTO profiles VALUES(3, 'Unknown', '{"upload":20,"download":30}', NULL);
	INSERT INTO profiles VALUES(4, 'Missing', NULL, NULL);
	INSERT INTO profiles VALUES(5, 'Corrupt', 'not json', NULL);
	INSERT INTO profiles VALUES(6, 'Negative', '{"upload":-1}', NULL);
	INSERT INTO profiles VALUES(7, 'Overflow', '{"upload":9223372036854775807,"download":1}', NULL);`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRAFFIC_MONITOR_FLCLASH_DB", path)
	s := newTestService(t)
	for _, tc := range []struct {
		id              int
		status          int
		used, remaining int64
	}{
		{1, 200, 50, 50}, {2, 200, 120, 0}, {3, 200, 50, 0},
		{4, 503, 0, 0}, {5, 503, 0, 0}, {6, 503, 0, 0}, {7, 503, 0, 0}, {99, 503, 0, 0},
	} {
		t.Run(strconv.Itoa(tc.id), func(t *testing.T) {
			t.Setenv("TRAFFIC_MONITOR_FLCLASH_PROFILE_ID", strconv.Itoa(tc.id))
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/subscription", nil))
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), path) {
				t.Fatal("response leaks database path")
			}
			if tc.status == 200 {
				var got flClashSubscription
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if !got.Configured || got.Used != tc.used || got.Remaining != tc.remaining {
					t.Fatalf("unexpected quota: %+v", got)
				}
				if tc.id == 1 && (got.Name != "当前订阅" || got.UpdatedAt != 1800000000 || got.Expire != 1900000000) {
					t.Fatalf("unexpected metadata: %+v", got)
				}
			}
		})
	}
	// Each request must see a new FlClash sync rather than a startup snapshot.
	t.Setenv("TRAFFIC_MONITOR_FLCLASH_PROFILE_ID", "1")
	if _, err := db.Exec(`UPDATE profiles SET subscription_info='{"upload":25,"download":50,"total":100}' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleFlClashSubscription(w, httptest.NewRequest(http.MethodGet, "/api/subscription", nil))
	var got flClashSubscription
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Remaining != 25 {
		t.Fatalf("sync not reflected: %+v", got)
	}
	w = httptest.NewRecorder()
	s.handleFlClashSubscription(w, httptest.NewRequest(http.MethodPost, "/api/subscription", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal(w.Code)
	}
	t.Setenv("TRAFFIC_MONITOR_FLCLASH_DB", "")
	w = httptest.NewRecorder()
	s.handleFlClashSubscription(w, httptest.NewRequest(http.MethodGet, "/api/subscription", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":false`) {
		t.Fatalf("disabled: %s", w.Body.String())
	}
	// Read-only mode must never create an empty FlClash database on a typo.
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	t.Setenv("TRAFFIC_MONITOR_FLCLASH_DB", missing)
	w = httptest.NewRecorder()
	s.handleFlClashSubscription(w, httptest.NewRequest(http.MethodGet, "/api/subscription", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("created missing database")
	}
}
