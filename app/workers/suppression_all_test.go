package workers

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"mimamori/app/models"
)

// Creating a suppression marks every matching unacknowledged detection of a
// target, and deleting it releases every one it suppressed, however many
// there are: here more than 100000, and one acknowledged and one of another
// executable that stay as they are.
func TestSuppressionCoversEveryDetection(t *testing.T) {
	e := newTestEnv(t)
	b, _ := models.GetBuiltinAgent(e.c.db.Read())
	tdb, err := e.c.targetDB(b)
	if err != nil {
		t.Fatal(err)
	}
	const n = 100037
	var ackedID, otherID int64
	if err := tdb.Write(func(tx models.Writer) error {
		for i := range n + 2 {
			d := &models.Detection{Fingerprint: fmt.Sprintf("fp-%d", i), Type: "exec", Category: "alert", OSUser: "nginx",
				Exe: "/usr/bin/zz", Target: fmt.Sprintf("zz %d", i), Count: 1, FirstAt: int64(1000 + i), LastAt: int64(1000 + i),
				NotifyState: models.NotifyPending}
			if i == n+1 {
				d.Exe = "/usr/bin/other"
			}
			if err := models.InsertDetection(tx, d); err != nil {
				return err
			}
			switch i {
			case n:
				ackedID = d.ID
			case n + 1:
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
