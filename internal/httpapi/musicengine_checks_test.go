package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/audiocheck"
	"github.com/tristenlammi/arrmada/internal/auth"
	"github.com/tristenlammi/arrmada/internal/settings"
)

// ---- settings ----

func TestMusicEngineSettingsDefaultsAndNormalisedPlaylist(t *testing.T) {
	s := newMEServer(t, &MusicEngineAPI{})
	_, admin := s.user(t, "admin", auth.RoleAdmin)

	def := meJSON(t, s.do("GET", meBase+"/settings", admin), 200)
	if def["watched_playlist"] != "5hLOEmkXHaXQDDbvoFFLX8" || def["floor_gb"] != float64(100) || def["margin_gb"] != float64(25) ||
		def["quarantine_share_gb"] != float64(200) || def["auto_swaps"] != false || def["enabled"] != false {
		t.Errorf("defaults = %v", def)
	}

	for _, in := range []string{
		"37i9dQZF1DXcBWIGoYBM5M",
		"spotify:playlist:37i9dQZF1DXcBWIGoYBM5M",
		"https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M?si=abc",
	} {
		got := meJSON(t, s.doJSON("PUT", meBase+"/settings", admin, fmt.Sprintf(`{"watched_playlist":%q}`, in)), 200)
		if got["watched_playlist"] != "37i9dQZF1DXcBWIGoYBM5M" {
			t.Errorf("playlist %q stored as %v, want the bare id", in, got["watched_playlist"])
		}
	}
	for _, bad := range []string{`{"watched_playlist":"not a playlist!"}`, `{"watched_playlist":""}`, `{"floor_gb":-1}`, `{"quarantine_share_gb":0}`, `{"nonsense":1}`} {
		if rec := s.doJSON("PUT", meBase+"/settings", admin, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s: HTTP %d, want 400", bad, rec.Code)
		}
	}
	got := meJSON(t, s.doJSON("PUT", meBase+"/settings", admin, `{"floor_gb":50,"margin_gb":10.5,"quarantine_share_gb":120,"enabled":true}`), 200)
	if got["floor_gb"] != float64(50) || got["margin_gb"] != 10.5 || got["quarantine_share_gb"] != float64(120) || got["enabled"] != true ||
		got["watched_playlist"] != "37i9dQZF1DXcBWIGoYBM5M" {
		t.Errorf("after partial update = %v (unsent fields must be kept)", got)
	}
	if v := s.deps.Settings.Get(t.Context(), MusicEngineKeyFloorGB, ""); v != "50" {
		t.Errorf("stored key %s = %q, want 50", MusicEngineKeyFloorGB, v)
	}
}

func meRecordPathTest(t *testing.T, s *routeServer, c *http.Cookie, passed bool) {
	t.Helper()
	meJSON(t, s.doJSON("POST", meBase+"/release-checks/path-test", c,
		fmt.Sprintf(`{"passed":%v,"summary":"rating 8, 12 plays, 3 playlists before and after"}`, passed)), 200)
}

// meCalibrationFixture is a library folder with a genuine, a fake and a quiet file, and a
// checker that classifies them as named.
func meCalibrationFixture(t *testing.T) (root string, checker *meFakeChecker, rows string) {
	t.Helper()
	root = t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good, fake := mk("genuine.flac"), mk("fake.flac")
	checker = &meFakeChecker{verdicts: map[string]audiocheck.Verdict{good: audiocheck.Genuine, fake: audiocheck.Fake}}
	rows = fmt.Sprintf(`{"rows":[{"path":%q,"expected":"GENUINE"},{"path":%q,"expected":"FAKE"}]}`, good, fake)
	return root, checker, rows
}

// Automatic swaps are refused unless BOTH release checks are recorded as passed, on the
// server; with both they turn on, and the answer survives a new api and a restart.
func TestMusicEngineAutoSwapsNeedBothReleaseChecks(t *testing.T) {
	root, checker, rows := meCalibrationFixture(t)
	m := &MusicEngineAPI{Checker: checker, LibraryRoot: func() string { return root }}
	s := newMEServer(t, m)
	_, admin := s.user(t, "admin", auth.RoleAdmin)
	put := func() int { return s.doJSON("PUT", meBase+"/settings", admin, `{"auto_swaps":true}`).Code }

	// Neither.
	rec := s.doJSON("PUT", meBase+"/settings", admin, `{"auto_swaps":true}`)
	body := meJSON(t, rec, http.StatusConflict)
	if body["error"] != "automatic swaps stay off until both release checks pass" {
		t.Errorf("refusal = %v", body)
	}
	// Only the path test.
	meRecordPathTest(t, s, admin, true)
	if c := put(); c != http.StatusConflict {
		t.Errorf("only the path test passed: HTTP %d, want 409", c)
	}
	// Only the calibration (a fresh server: path test not recorded).
	s2 := newMEServer(t, &MusicEngineAPI{Checker: checker, LibraryRoot: func() string { return root }})
	_, admin2 := s2.user(t, "admin", auth.RoleAdmin)
	meJSON(t, s2.doJSON("POST", meBase+"/release-checks/calibration", admin2, rows), 200)
	if c := s2.doJSON("PUT", meBase+"/settings", admin2, `{"auto_swaps":true}`).Code; c != http.StatusConflict {
		t.Errorf("only the calibration passed: HTTP %d, want 409", c)
	}
	if got := meJSON(t, s2.do("GET", meBase+"/settings", admin2), 200); got["auto_swaps"] != false {
		t.Errorf("a refused save changed the setting: %v", got)
	}
	// A path test recorded as FAILED does not count.
	meRecordPathTest(t, s2, admin2, false)
	if c := s2.doJSON("PUT", meBase+"/settings", admin2, `{"auto_swaps":true}`).Code; c != http.StatusConflict {
		t.Errorf("failed path test: HTTP %d, want 409", c)
	}

	// Both.
	meJSON(t, s.doJSON("POST", meBase+"/release-checks/calibration", admin, rows), 200)
	got := meJSON(t, s.doJSON("PUT", meBase+"/settings", admin, `{"auto_swaps":true}`), 200)
	if got["auto_swaps"] != true {
		t.Fatalf("with both checks: %v", got)
	}
	if !MusicEngineAutoSwaps(t.Context(), s.deps.Settings) {
		t.Error("MusicEngineAutoSwaps = false with the setting on and both checks passed")
	}

	// A new api instance over the same settings sees it...
	s.mountME(&MusicEngineAPI{DB: s.st.DB()})
	if got := meJSON(t, s.do("GET", meBase+"/settings", admin), 200); got["auto_swaps"] != true {
		t.Errorf("after a new api instance: %v", got)
	}
	// ...and so does a restart: a new settings service loaded from the same database.
	restarted := settings.NewService(s.st.DB())
	if err := restarted.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !MusicEngineAutoSwaps(t.Context(), restarted) {
		t.Error("automatic swaps are not on after a restart")
	}
	checks := meJSON(t, s.do("GET", meBase+"/release-checks", admin), 200)
	for _, k := range []string{"calibration", "path_test"} {
		c := checks[k].(map[string]any)
		if c["passed"] != true || c["at"] == nil || c["summary"] == "" {
			t.Errorf("%s after restart = %v", k, c)
		}
	}

	// Recording a failed check switches automatic swaps off again.
	meRecordPathTest(t, s, admin, false)
	if got := meJSON(t, s.do("GET", meBase+"/settings", admin), 200); got["auto_swaps"] != false {
		t.Errorf("a failed check left automatic swaps on: %v", got)
	}
	if MusicEngineAutoSwaps(t.Context(), s.deps.Settings) {
		t.Error("MusicEngineAutoSwaps stayed true after a failed check")
	}
}

// A hand-edited setting cannot turn swaps on without the checks: the sweep's rule needs both.
func TestMusicEngineAutoSwapsRuleIgnoresAHandEditedFlag(t *testing.T) {
	s := newMEServer(t, &MusicEngineAPI{})
	if err := s.deps.Settings.SetBool(t.Context(), MusicEngineKeyAutoSwaps, true); err != nil {
		t.Fatal(err)
	}
	if MusicEngineAutoSwaps(t.Context(), s.deps.Settings) {
		t.Error("automatic swaps ran on a flag alone")
	}
	_, admin := s.user(t, "admin", auth.RoleAdmin)
	if got := meJSON(t, s.do("GET", meBase+"/settings", admin), 200); got["auto_swaps"] != false {
		t.Errorf("settings view shows swaps on without checks: %v", got)
	}
}

// ---- release checks ----

func TestMusicEngineCalibrationReportsEachPathAndOverall(t *testing.T) {
	root, checker, rows := meCalibrationFixture(t)
	s := newMEServer(t, &MusicEngineAPI{Checker: checker, LibraryRoot: func() string { return root }, Now: func() time.Time { return time.Unix(5000, 0) }})
	_, admin := s.user(t, "admin", auth.RoleAdmin)

	if got := meJSON(t, s.do("GET", meBase+"/release-checks", admin), 200); got["calibration"].(map[string]any)["passed"] != nil {
		t.Errorf("before any run: %v, want passed null", got)
	}

	res := meJSON(t, s.doJSON("POST", meBase+"/release-checks/calibration", admin, rows), 200)
	if res["passed"] != true || len(res["rows"].([]any)) != 2 {
		t.Fatalf("passing run = %v", res)
	}
	r0 := res["rows"].([]any)[0].(map[string]any)
	if r0["expected"] != "GENUINE" || r0["actual"] != "GENUINE" || r0["ok"] != true {
		t.Errorf("row = %v", r0)
	}
	rc := meJSON(t, s.do("GET", meBase+"/release-checks", admin), 200)["calibration"].(map[string]any)
	if rc["passed"] != true || rc["at"] != float64(5000) || !strings.Contains(fmt.Sprint(rc["summary"]), "2 of 2") {
		t.Errorf("recorded calibration = %v", rc)
	}

	// The checker calls the fake file genuine: the run fails, saying which file and how.
	checker.verdicts[filepath.Join(root, "fake.flac")] = audiocheck.Genuine
	res = meJSON(t, s.doJSON("POST", meBase+"/release-checks/calibration", admin, rows), 200)
	if res["passed"] != false {
		t.Fatalf("a misclassified file passed the run: %v", res)
	}
	r1 := res["rows"].([]any)[1].(map[string]any)
	if r1["expected"] != "FAKE" || r1["actual"] != "GENUINE" || r1["ok"] != false {
		t.Errorf("mismatched row = %v", r1)
	}
	rc = meJSON(t, s.do("GET", meBase+"/release-checks", admin), 200)["calibration"].(map[string]any)
	if rc["passed"] != false || !strings.Contains(fmt.Sprint(rc["summary"]), "fake.flac") {
		t.Errorf("recorded failure = %v", rc)
	}

	// A file the checker cannot read is Blind, which is a failure, never a pass.
	delete(checker.verdicts, filepath.Join(root, "genuine.flac"))
	if res := meJSON(t, s.doJSON("POST", meBase+"/release-checks/calibration", admin, rows), 200); res["passed"] != false {
		t.Errorf("a blind file passed: %v", res)
	}
}

func TestMusicEngineCalibrationRefusesBadRows(t *testing.T) {
	root, checker, _ := meCalibrationFixture(t)
	outside := t.TempDir()
	escape := filepath.Join(root, "escape.flac")
	if err := os.Symlink(filepath.Join(outside, "elsewhere.flac"), escape); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "elsewhere.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newMEServer(t, &MusicEngineAPI{Checker: checker, LibraryRoot: func() string { return root }})
	_, admin := s.user(t, "admin", auth.RoleAdmin)
	good := filepath.Join(root, "genuine.flac")
	post := func(body string) int {
		return s.doJSON("POST", meBase+"/release-checks/calibration", admin, body).Code
	}

	for name, body := range map[string]string{
		"outside the library":       fmt.Sprintf(`{"rows":[{"path":%q,"expected":"GENUINE"},{"path":"/etc/passwd","expected":"FAKE"}]}`, good),
		"dot-dot out of the root":   fmt.Sprintf(`{"rows":[{"path":%q,"expected":"GENUINE"},{"path":%q,"expected":"FAKE"}]}`, good, root+"/../etc/passwd"),
		"a sibling with its prefix": fmt.Sprintf(`{"rows":[{"path":%q,"expected":"GENUINE"},{"path":%q,"expected":"FAKE"}]}`, good, root+"-other/x.flac"),
		"a symlink out":             fmt.Sprintf(`{"rows":[{"path":%q,"expected":"GENUINE"},{"path":%q,"expected":"FAKE"}]}`, good, escape),
		"a relative path":           fmt.Sprintf(`{"rows":[{"path":%q,"expected":"GENUINE"},{"path":"fake.flac","expected":"FAKE"}]}`, good),
		"the root itself":           fmt.Sprintf(`{"rows":[{"path":%q,"expected":"GENUINE"},{"path":%q,"expected":"FAKE"}]}`, good, root),
		"an unknown verdict":        fmt.Sprintf(`{"rows":[{"path":%q,"expected":"MAYBE"}]}`, good),
		"only genuine files":        fmt.Sprintf(`{"rows":[{"path":%q,"expected":"GENUINE"}]}`, good),
		"no rows":                   `{"rows":[]}`,
	} {
		if c := post(body); c != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d, want 400", name, c)
		}
	}
	if len(checker.calls) != 0 {
		t.Errorf("the checker ran on a refused request: %v", checker.calls)
	}
	if got := meJSON(t, s.do("GET", meBase+"/release-checks", admin), 200)["calibration"].(map[string]any); got["passed"] != nil {
		t.Errorf("a refused run was recorded: %v", got)
	}

	// Blank and # rows (a pasted list) are skipped, not errors.
	body := fmt.Sprintf(`{"rows":[{"path":"","expected":""},{"path":"# known good","expected":""},{"path":%q,"expected":"genuine"},{"path":%q,"expected":"FAKE"}]}`,
		good, filepath.Join(root, "fake.flac"))
	if res := meJSON(t, s.doJSON("POST", meBase+"/release-checks/calibration", admin, body), 200); res["passed"] != true || len(res["rows"].([]any)) != 2 {
		t.Errorf("run with comment rows = %v", res)
	}
}

func TestMusicEnginePathTestIsRecordedAndNeedsEvidenceToPass(t *testing.T) {
	s := newMEServer(t, &MusicEngineAPI{Now: func() time.Time { return time.Unix(9000, 0) }})
	_, admin := s.user(t, "admin", auth.RoleAdmin)

	if rec := s.doJSON("POST", meBase+"/release-checks/path-test", admin, `{"passed":true,"summary":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a pass with no summary: HTTP %d, want 400", rec.Code)
	}
	got := meJSON(t, s.doJSON("POST", meBase+"/release-checks/path-test", admin, `{"passed":true,"summary":"rating 8 -> 8, plays 12 -> 12"}`), 200)
	pt := got["path_test"].(map[string]any)
	if pt["passed"] != true || pt["at"] != float64(9000) || pt["summary"] != "rating 8 -> 8, plays 12 -> 12" || got["calibration"].(map[string]any)["passed"] != nil {
		t.Errorf("recorded = %v", got)
	}
	// Survives a restart.
	restarted := settings.NewService(s.st.DB())
	if err := restarted.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !meCheckPassed(t.Context(), restarted, MusicEngineKeyPathTest, nil) {
		t.Error("the path test result did not survive a restart")
	}
	// A record that cannot be read counts as not run, never as passed.
	if err := s.deps.Settings.Set(t.Context(), MusicEngineKeyPathTest, "{garbage"); err != nil {
		t.Fatal(err)
	}
	if got := meJSON(t, s.do("GET", meBase+"/release-checks", admin), 200)["path_test"].(map[string]any); got["passed"] != nil {
		t.Errorf("unreadable record = %v, want passed null", got)
	}
}
