package musicengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tristenlammi/arrmada/internal/diskspace"
)

const (
	spaceGiB    = int64(1 << 30)
	spaceLib    = "/lib"
	spaceDl     = "/dl"
	spaceFloor  = 100 * spaceGiB
	spaceMargin = 25 * spaceGiB
)

// spaceFakeVolumes is the injected measure func: free bytes per path, or unmeasurable.
type spaceFakeVolumes struct {
	mu      sync.Mutex
	free    map[string]int64
	missing map[string]bool
	block   chan struct{} // when set, measuring blocks until it is closed
}

func (f *spaceFakeVolumes) measure(path string) (diskspace.Usage, bool) {
	f.mu.Lock()
	block, miss, free := f.block, f.missing[path], f.free[path]
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if miss {
		return diskspace.Usage{}, false
	}
	return diskspace.Usage{TotalBytes: uint64(1000 * spaceGiB), FreeBytes: uint64(free)}, true
}

func (f *spaceFakeVolumes) set(lib, dl int64) {
	f.mu.Lock()
	f.free = map[string]int64{spaceLib: lib * spaceGiB, spaceDl: dl * spaceGiB}
	f.mu.Unlock()
}

func (f *spaceFakeVolumes) setMissing(path string, missing bool) {
	f.mu.Lock()
	if f.missing == nil {
		f.missing = map[string]bool{}
	}
	f.missing[path] = missing
	f.mu.Unlock()
}

func (v *env) newSpace(vols *spaceFakeVolumes) *Space {
	v.t.Helper()
	s, err := NewSpace(SpaceConfig{
		DB: v.db, LibraryPath: spaceLib, DownloadsPath: spaceDl,
		Settings: func() SpaceSettings {
			return SpaceSettings{Floor: spaceFloor, Margin: spaceMargin, QuarantineShare: 200 * spaceGiB}
		},
		Measure: vols.measure, MeasureTimeout: 2 * time.Second,
		Notifier: v.notifier, Now: v.clock.Now,
	})
	if err != nil {
		v.t.Fatal(err)
	}
	return s
}

func spaceAllow(t *testing.T, s *Space, kind WorkKind) (bool, string) {
	t.Helper()
	return s.Allow(context.Background(), kind)
}

func spaceAlerts(v *env) int { return v.notifier.count(AlertPausedForSpace) }

// Free space below the floor pauses the sweep first; a reservation that would cross the
// floor pauses intake too; nothing is deleted (AE14).
func TestSpaceSweepPausesFirstThenIntakeOnAReservation(t *testing.T) {
	v := newEnv(t)
	vols := &spaceFakeVolumes{}
	s := v.newSpace(vols)
	keep := filepath.Join(t.TempDir(), "library-file.flac")
	if err := os.WriteFile(keep, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}

	vols.set(300, 300)
	if ok, why := spaceAllow(t, s, WorkSweep); !ok {
		t.Fatalf("sweep with 300 GB free: %q", why)
	}
	if ok, why := spaceAllow(t, s, WorkIntake); !ok {
		t.Fatalf("intake with 300 GB free: %q", why)
	}

	// 120 GB free is above the floor but inside floor + margin: the sweep stops, intake runs.
	vols.set(120, 300)
	ok, why := spaceAllow(t, s, WorkSweep)
	if ok || !strings.Contains(why, "/lib") && !strings.Contains(why, "120.0 GB") {
		t.Fatalf("sweep at 120 GB: ok=%v reason=%q, want paused with the measured figure", ok, why)
	}
	if ok, why := spaceAllow(t, s, WorkIntake); !ok {
		t.Fatalf("intake at 120 GB should still run: %q", why)
	}
	if spaceAlerts(v) != 1 {
		t.Fatalf("alerts after the sweep paused = %d, want 1", spaceAlerts(v))
	}

	// A 30 GB download would leave 90 GB: refused, and intake pauses with it.
	item := v.seedItem(KindRequest, StateSearching, 0)
	if ok, reason := s.Reserve(context.Background(), item, 30*spaceGiB); ok || !strings.Contains(reason, "floor") {
		t.Fatalf("Reserve crossing the floor: ok=%v reason=%q", ok, reason)
	}
	if ok, _ := spaceAllow(t, s, WorkIntake); ok {
		t.Fatal("intake should be paused after a reservation crossed the floor")
	}
	// A smaller one that fits was fine before; now the pause holds until space is back.
	if spaceAlerts(v) != 2 {
		t.Fatalf("alerts = %d, want 2 (sweep, then intake)", spaceAlerts(v))
	}
	for i := 0; i < 5; i++ {
		spaceAllow(t, s, WorkIntake)
		spaceAllow(t, s, WorkSweep)
	}
	if spaceAlerts(v) != 2 {
		t.Fatalf("repeated checks raised more alerts: %d, want still 2", spaceAlerts(v))
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("a pause must not delete anything: %v", err)
	}
}

// New music resumes at floor + margin, before the sweep, which needs floor + 2*margin.
func TestSpaceResumesIntakeBeforeSweep(t *testing.T) {
	v := newEnv(t)
	vols := &spaceFakeVolumes{}
	s := v.newSpace(vols)

	vols.set(90, 300)
	spaceAllow(t, s, WorkIntake)
	if ok, _ := spaceAllow(t, s, WorkIntake); ok {
		t.Fatal("intake should be paused at 90 GB")
	}
	if ok, _ := spaceAllow(t, s, WorkSweep); ok {
		t.Fatal("sweep should be paused at 90 GB")
	}

	vols.set(110, 300) // back above the floor, not yet floor + margin: still paused
	if ok, _ := spaceAllow(t, s, WorkIntake); ok {
		t.Fatal("intake must stay paused between the floor and floor + margin")
	}

	vols.set(126, 300) // floor + margin
	if ok, why := spaceAllow(t, s, WorkIntake); !ok {
		t.Fatalf("intake should resume at floor + margin: %q", why)
	}
	if ok, _ := spaceAllow(t, s, WorkSweep); ok {
		t.Fatal("the sweep should still be paused at floor + margin")
	}

	vols.set(151, 300) // floor + 2*margin
	if ok, why := spaceAllow(t, s, WorkSweep); !ok {
		t.Fatalf("the sweep should resume at floor + 2*margin: %q", why)
	}
}

// The lower of the two volumes decides: a full library volume pauses work even when the
// downloads volume has room.
func TestSpaceMeasuresEachVolume(t *testing.T) {
	v := newEnv(t)
	vols := &spaceFakeVolumes{}
	s := v.newSpace(vols)
	vols.set(500, 60)
	if ok, _ := spaceAllow(t, s, WorkIntake); ok {
		t.Fatal("intake should pause when the downloads volume is under the floor")
	}
	vols.set(60, 500)
	spaceAllow(t, s, WorkIntake) // still paused; recovery needs floor + margin on both
	if ok, _ := spaceAllow(t, s, WorkIntake); ok {
		t.Fatal("intake should stay paused while the library volume is under the floor")
	}
}

// An unmeasurable volume pauses both kinds of work and raises exactly one alert.
func TestSpaceUnmeasurableVolumePausesBothWithOneAlert(t *testing.T) {
	v := newEnv(t)
	vols := &spaceFakeVolumes{}
	s := v.newSpace(vols)
	vols.set(500, 500)
	vols.setMissing(spaceDl, true)
	for i := 0; i < 4; i++ {
		for _, k := range []WorkKind{WorkIntake, WorkSweep} {
			if ok, why := spaceAllow(t, s, k); ok || !strings.Contains(why, spaceDl) {
				t.Fatalf("kind %v with an unmeasurable volume: ok=%v reason=%q", k, ok, why)
			}
		}
	}
	if spaceAlerts(v) != 1 {
		t.Fatalf("alerts = %d, want exactly 1", spaceAlerts(v))
	}
	// The volume coming back with plenty of room resumes both.
	vols.setMissing(spaceDl, false)
	if ok, why := spaceAllow(t, s, WorkIntake); !ok {
		t.Fatalf("intake after the volume answered: %q", why)
	}
	if ok, why := spaceAllow(t, s, WorkSweep); !ok {
		t.Fatalf("sweep after the volume answered: %q", why)
	}
}

// A volume that does not answer in time (a dead NFS mount) counts as unmeasurable.
func TestSpaceMeasureTimeoutCountsAsUnmeasurable(t *testing.T) {
	v := newEnv(t)
	vols := &spaceFakeVolumes{block: make(chan struct{})}
	t.Cleanup(func() { close(vols.block) })
	vols.set(500, 500)
	s, err := NewSpace(SpaceConfig{
		DB: v.db, LibraryPath: spaceLib, DownloadsPath: spaceDl, Measure: vols.measure,
		MeasureTimeout: 30 * time.Millisecond, Notifier: v.notifier, Now: v.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if ok, _ := spaceAllow(t, s, WorkIntake); ok {
		t.Fatal("a hung volume must pause intake")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Allow took %s; the measurement needs a hard deadline", time.Since(start))
	}
}

// Reservations are held per item, counted against free space, replaced on a second call
// and given back by Release; a gate restarted mid-download rebuilds them from the database.
func TestSpaceReservations(t *testing.T) {
	v := newEnv(t)
	vols := &spaceFakeVolumes{}
	s := v.newSpace(vols)
	ctx := context.Background()
	vols.set(200, 200)
	a := v.seedItem(KindRequest, StateSearching, 0)
	b := v.seedItem(KindRequest, StateSearching, v.seedRecording("r-b", "B", "X", "", ""))

	if ok, why := s.Reserve(ctx, a, 30*spaceGiB); !ok {
		t.Fatalf("first reservation: %q", why)
	}
	if ok, _ := s.Reserve(ctx, b, 80*spaceGiB); ok {
		t.Fatal("200 - 30 - 80 = 90 GB is under the floor and must be refused")
	}
	// Refused: B holds nothing, and the pause now holds until space returns.
	vols.set(150, 150) // 150 - 30 held = 120: above the floor, under floor + margin
	if ok, _ := s.Reserve(ctx, b, 1*spaceGiB); ok {
		t.Fatal("intake stays paused after a refused reservation until floor + margin free")
	}
	vols.set(600, 600)
	if ok, why := s.Reserve(ctx, b, 80*spaceGiB); !ok {
		t.Fatalf("after space returned: %q", why)
	}
	// Reserving again replaces, not adds: 600 - 30 - 80 = 490, then B at 10 GB.
	if ok, _ := s.Reserve(ctx, b, 10*spaceGiB); !ok {
		t.Fatal("re-reserving a smaller amount must be allowed")
	}
	st, err := s.Status(ctx)
	if err != nil || st.ReservedBytes != 40*spaceGiB {
		t.Fatalf("held = %d, %v; want 40 GiB", st.ReservedBytes, err)
	}
	s.Release(ctx, a)
	s.Release(ctx, a) // releasing twice is harmless
	if st, _ = s.Status(ctx); st.ReservedBytes != 10*spaceGiB {
		t.Fatalf("held after release = %d, want 10 GiB", st.ReservedBytes)
	}
	if ok, _ := s.Reserve(ctx, b, 0); ok {
		t.Fatal("a zero-byte reservation is a bug in the caller, not a free pass")
	}

	// Restart: a downloading candidate's reserved bytes are counted again.
	if _, err := v.db.Exec(`INSERT INTO mus_candidates (item_id, lane, state, reserved_bytes, created_at) VALUES (?, 'usenet', 'downloading', ?, ?)`,
		a, 500*spaceGiB, v.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	fresh := v.newSpace(vols)
	vols.set(560, 560)
	if ok, _ := spaceAllow(t, fresh, WorkIntake); ok {
		t.Fatal("560 GB free less a 500 GB reservation recovered from the database is 60 GB: paused")
	}
}

// A sweep item's reservation obeys the sweep's stricter threshold.
func TestSpaceSweepReservationRefusedWhileSweepPaused(t *testing.T) {
	v := newEnv(t)
	vols := &spaceFakeVolumes{}
	s := v.newSpace(vols)
	vols.set(120, 120)
	sweep := v.seedItem(KindSweep, StateSearching, 0)
	if ok, _ := s.Reserve(context.Background(), sweep, 1*spaceGiB); ok {
		t.Fatal("a sweep download inside floor + margin must wait")
	}
	intake := v.seedItem(KindRequest, StateSearching, v.seedRecording("r-i", "I", "X", "", ""))
	if ok, why := s.Reserve(context.Background(), intake, 1*spaceGiB); !ok {
		t.Fatalf("intake with room above the floor: %q", why)
	}
}

// A full quarantine share pauses only the sweep (R34, AE16), with one alert; clearing
// versions resumes it.
func TestSpaceQuarantineShareFullPausesOnlyTheSweep(t *testing.T) {
	v := newEnv(t)
	vols := &spaceFakeVolumes{}
	s := v.newSpace(vols)
	vols.set(900, 900)
	for i, gb := range []int64{120, 80} {
		if _, err := v.db.Exec(`INSERT INTO mus_quarantine (path, size_bytes, created_at) VALUES (?, ?, ?)`,
			"/q/"+string(rune('a'+i)), gb*spaceGiB, v.clock.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if ok, why := spaceAllow(t, s, WorkSweep); ok || !strings.Contains(why, "quarantine") {
			t.Fatalf("sweep with a full quarantine: ok=%v reason=%q", ok, why)
		}
		if ok, why := spaceAllow(t, s, WorkIntake); !ok {
			t.Fatalf("intake must continue while quarantine is full: %q", why)
		}
	}
	if n := v.notifier.count(AlertQuarantineFull); n != 1 {
		t.Fatalf("quarantine alerts = %d, want 1", n)
	}
	if spaceAlerts(v) != 0 {
		t.Fatal("a full quarantine is not a space pause")
	}
	st, err := s.Status(context.Background())
	if err != nil || !st.SweepPaused || st.IntakePaused || st.QuarantineUsed != 200*spaceGiB {
		t.Fatalf("status = %+v, %v", st, err)
	}

	if _, err := v.db.Exec(`UPDATE mus_quarantine SET cleared_at = ? WHERE path = '/q/a'`, v.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if ok, why := spaceAllow(t, s, WorkSweep); !ok {
		t.Fatalf("sweep after Blair cleared versions: %q", why)
	}
}

// A gate with no volume to look at cannot decide, so it says no.
func TestSpaceWithNoVolumesSaysNo(t *testing.T) {
	v := newEnv(t)
	s, err := NewSpace(SpaceConfig{DB: v.db, Notifier: v.notifier, Now: v.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := spaceAllow(t, s, WorkIntake); ok {
		t.Fatal("no configured volume must mean paused")
	}
}
