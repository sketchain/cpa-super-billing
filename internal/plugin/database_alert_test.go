package plugin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"cpa-super-billing/internal/billing"
)

type alertClock struct{ now time.Time }

func (c *alertClock) Now() time.Time { return c.now }

func newAlertTestApp(t *testing.T) (*App, string, *alertClock) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the write-ahead log file check is off on Windows, which cannot delete open files")
	}
	app, path := newAppWithPriceAndState(t, true)
	clock := &alertClock{now: time.Now().UTC()}
	app.store.SetClock(clock.Now)
	return app, path, clock
}

func databaseAlertLogs(t *testing.T, app *App) (errors, infos int) {
	t.Helper()
	page, err := app.store.PluginLogsPage(billing.PluginLogQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range page.Entries {
		if !strings.Contains(entry.Message, "write-ahead log") {
			continue
		}
		switch entry.Level {
		case billing.PluginLogError:
			errors++
		case billing.PluginLogInfo:
			infos++
		}
	}
	return errors, infos
}

func alertHeader(t *testing.T, app *App) string {
	t.Helper()
	return callManagement(t, app, http.MethodGet, routeKeys, nil, nil).Headers.Get(databaseAlertHeader)
}

// requestEventsInDatabaseFile counts the request events the database file
// alone holds, read from a copy without any write-ahead log.
func requestEventsInDatabaseFile(t *testing.T, path string) int {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(t.TempDir(), "copied.db")
	if err := os.WriteFile(copied, content, 0o600); err != nil {
		t.Fatal(err)
	}
	handle, err := sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: copied, OmitHost: true}).String()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	var count int
	if err := handle.QueryRow("SELECT count(*) FROM request_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestDatabaseAlertWhenWALFilesAreDeleted(t *testing.T) {
	app, path, clock := newAlertTestApp(t)
	if header := alertHeader(t, app); header != "" {
		t.Fatalf("alert header on a healthy database = %q", header)
	}
	for _, name := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	// The check above ran a moment ago, so this one is throttled.
	billOneRequest(t, app, "sk-alert-0001", 10)
	if header := alertHeader(t, app); header != "" {
		t.Fatalf("alert header within the check interval = %q", header)
	}
	if errors, _ := databaseAlertLogs(t, app); errors != 0 {
		t.Fatalf("alert logged within the check interval")
	}

	clock.now = clock.now.Add(billing.DatabaseFileCheckInterval)
	billOneRequest(t, app, "sk-alert-0001", 10) // usage.handle runs the check
	if header := alertHeader(t, app); header != databaseAlertCheckpointed {
		t.Fatalf("alert header = %q, want %q", header, databaseAlertCheckpointed)
	}
	if errors, infos := databaseAlertLogs(t, app); errors != 1 || infos != 1 {
		t.Fatalf("alert logs = %d errors, %d infos; want one of each", errors, infos)
	}
	if count := requestEventsInDatabaseFile(t, path); count != 2 {
		t.Fatalf("request events in the database file = %d, want 2", count)
	}

	billOneRequest(t, app, "sk-alert-0001", 10)
	clock.now = clock.now.Add(billing.DatabaseFileCheckInterval)
	if header := alertHeader(t, app); header != databaseAlertCheckpointed {
		t.Fatalf("alert header after a second check = %q", header)
	}
	if count := requestEventsInDatabaseFile(t, path); count != 3 {
		t.Fatalf("request events in the database file after the second checkpoint = %d, want 3", count)
	}
	if errors, infos := databaseAlertLogs(t, app); errors != 1 || infos != 1 {
		t.Fatalf("alert logs after a second check = %d errors, %d infos; the alert must not repeat", errors, infos)
	}
	for _, name := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Errorf("the alert created %s; it must leave the files to the operator", name)
		}
	}

	var view struct {
		Alert struct {
			Path       string                 `json:"path"`
			Missing    []string               `json:"missing"`
			Checkpoint *billing.WALCheckpoint `json:"checkpoint"`
			Message    struct {
				Key    string            `json:"message_key"`
				Params map[string]string `json:"message_params"`
			} `json:"message"`
		} `json:"database_alert"`
	}
	resp := callManagement(t, app, http.MethodGet, routeCC, nil, nil)
	if err := json.Unmarshal(resp.Body, &view); err != nil {
		t.Fatal(err)
	}
	alert := view.Alert
	if alert.Path != path || len(alert.Missing) != 2 || alert.Checkpoint == nil || alert.Checkpoint.Busy != 0 ||
		alert.Message.Key != "backend.database_wal_deleted_checkpointed" || !strings.Contains(alert.Message.Params["v0"], path+"-wal") {
		t.Fatalf("database_alert = %+v", alert)
	}
}

func TestDatabaseAlertStaysOffInNormalUse(t *testing.T) {
	app, _, clock := newAlertTestApp(t)
	for index := range 5 {
		billOneRequest(t, app, "sk-normal-"+strconv.Itoa(index), 10)
		callOK(t, app, http.MethodGet, routeReferencePrices, url.Values{"q": {"gpt"}}, nil, http.StatusOK, nil)
		callOK(t, app, http.MethodGet, routeReferencePricesStatus, nil, nil, http.StatusOK, nil)
		if header := alertHeader(t, app); header != "" {
			t.Fatalf("alert header in normal use = %q", header)
		}
		clock.now = clock.now.Add(billing.DatabaseFileCheckInterval)
	}
	if errors, infos := databaseAlertLogs(t, app); errors != 0 || infos != 0 {
		t.Fatalf("alert logs in normal use = %d errors, %d infos", errors, infos)
	}
}

// Another database replaces the alert of the one it replaces, and its own
// files are watched from then on.
func TestDatabaseAlertFollowsTheConfiguredDatabase(t *testing.T) {
	app, path, clock := newAlertTestApp(t)
	alertHeader(t, app)
	for _, name := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	clock.now = clock.now.Add(billing.DatabaseFileCheckInterval)
	if header := alertHeader(t, app); header != databaseAlertCheckpointed {
		t.Fatalf("alert header = %q", header)
	}

	next := filepath.Join(t.TempDir(), "next.db")
	raw, err := app.HandleMethod(MethodPluginReconfigure, mustMarshal(t, LifecycleRequest{
		ConfigYAML: []byte("enabled: true\nstate_file: \"" + next + "\"\n"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, raw, nil)
	if header := alertHeader(t, app); header != "" {
		t.Fatalf("alert header after switching databases = %q", header)
	}
	clock.now = clock.now.Add(billing.DatabaseFileCheckInterval)
	billOneRequest(t, app, "sk-next-0001", 10)
	if header := alertHeader(t, app); header != "" {
		t.Fatalf("alert header on the new, healthy database = %q", header)
	}

	if err := os.Remove(next + "-wal"); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(billing.DatabaseFileCheckInterval)
	if header := alertHeader(t, app); header != databaseAlertCheckpointed {
		t.Fatalf("alert header after the new database's -wal was deleted = %q", header)
	}
}
