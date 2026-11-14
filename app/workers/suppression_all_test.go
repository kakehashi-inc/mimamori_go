package workers

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// Creating a suppression marks every matching unacknowledged detection of a
// target, and deleting it releases every one it suppressed, however many
// there are: here more than 100000, and one acknowledged and one of another
// executable that stay as they are. The program marks and releases the
// detections one statement each, which takes minutes under the race
// detector; there the test uses a little more than two pages
// (modules.DetectionPageRows) of detections, which runs the same code, and
// the run without it proves the 100000.
func TestSuppressionCoversEveryDetection(t *testing.T) {
	e := newTestEnv(t)
	b, _ := models.GetBuiltinAgent(e.c.db.Read())
	tdb, err := e.c.targetDB(b)
	if err != nil {
		t.Fatal(err)
	}
	n := 100037
	if raceEnabled {
		n = 2*modules.DetectionPageRows + 37
	}
	var ackedID, otherID int64
	if err := tdb.Write(func(tx models.Writer) error {
		// The n matching detections in one statement; the same columns as
		// models.InsertDetection writes.
		if _, err := tx.Exec(`INSERT INTO detections (fingerprint, type, category, os_user, exe, target, count, first_at, last_at,
			notify_state, created_at)
			WITH RECURSIVE seq(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i + 1 < ?)
			SELECT 'fp-' || i, 'exec', 'alert', 'nginx', '/usr/bin/zz', 'zz ' || i, 1, 1000 + i, 1000 + i, ?, ? FROM seq`,
			n, models.NotifyPending, models.Now()); err != nil {
			return err
		}
		for i, exe := range []string{"/usr/bin/zz", "/usr/bin/other"} {
			d := &models.Detection{Fingerprint: fmt.Sprintf("fp-%d", n+i), Type: "exec", Category: "alert", OSUser: "nginx",
				Exe: exe, Target: fmt.Sprintf("zz %d", n+i), Count: 1, FirstAt: int64(1000 + n + i), LastAt: int64(1000 + n + i),
				NotifyState: models.NotifyPending}
			if err := models.InsertDetection(tx, d); err != nil {
				return err
			}
			if i == 0 {
				ackedID = d.ID
			} else {
				otherID = d.ID
			}
		}
		_, err := models.AcknowledgeDetection(tx, ackedID, "test", 2000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	count := func(suppressed bool) int {
		f := false
		_, total, err := models.ListDetections(tdb.Read(), models.DetectionFilter{Acknowledged: &f, Suppressed: &suppressed}, 1)
		if err != nil {
			t.Fatal(err)
		}
		return total
	}
	lastHistory := func() string {
		hist, _, _ := models.ListHistory(e.c.db.Read(), models.HistSuppression, "", nil, 1, 0)
		if len(hist) == 0 {
			t.Fatal("no history")
		}
		return hist[0].Detail
	}

	s := e.admin("POST", "/suppressions", map[string]any{"exe": "/usr/bin/zz"})
	id := int64(s["id"].(float64))
	if got := count(true); got != n {
		t.Fatalf("suppressed after creating: %d, want %d", got, n)
	}
	if h := lastHistory(); !strings.Contains(h, strconv.Itoa(n)+" open detection(s) now suppressed") {
		t.Fatalf("history of the creation: %q", h)
	}
	for _, d := range []int64{ackedID, otherID} {
		if got, _ := models.GetDetection(tdb.Read(), d); got.Suppressed || got.NotifyState != models.NotifyPending {
			t.Fatalf("detection %d must stay as it was: %+v", d, got)
		}
	}

	e.admin("DELETE", "/suppressions/"+strconv.FormatInt(id, 10), nil)
	if got := count(true); got != 0 {
		t.Fatalf("still suppressed after deleting: %d", got)
	}
	if got := count(false); got != n+1 {
		t.Fatalf("not suppressed after deleting: %d, want %d", got, n+1)
	}
	if h := lastHistory(); !strings.Contains(h, strconv.Itoa(n)+" detection(s) no longer suppressed") {
		t.Fatalf("history of the deletion: %q", h)
	}
}
