package modules

import (
	"encoding/json"
	"strconv"

	"mimamori/app/models"
)

// The outbox of the agent: the reports that wait for the central, in the
// table outbox of the agent's local database. It is the only owner of that
// table: every change goes through its methods, each one Write. Reports
// about command sets are queued by queue (from any flow), the report of a
// monitoring step by queuePeriodic; only the monitoring step and the final
// send at the stop call flush, never at the same time, so reports leave one
// at a time in the order they were queued.
//
// The content of a queued row only shrinks: host information is removed
// from all but the newest periodic row, and the limits drop output, runs
// and detections. A row is never given more content.
//
// Every row carries its own report identity (ReportRequest.ReportID), made
// when the report is first written here and kept in the stored payload: a
// limit that rewrites the row, a send that fails and a restart of the agent
// do not change it, so a report sent again is the same report for the
// central.

// outbox stores and sends the agent's reports.
type outbox struct {
	db *DB
	// limit is the bound of the queued command set reports
	// (MaxOutboxCommandBytes, or the test value).
	limit int64
	// logf writes the agent's log lines about dropped content.
	logf func(format string, a ...any)
}

// newOutbox returns the outbox of the local database db.
func newOutbox(db *DB, logf func(format string, a ...any)) *outbox {
	return &outbox{db: db, limit: MaxOutboxCommandBytes, logf: logf}
}

// queue stores a report about command sets: it is inserted and the limit of
// command set reports applied in one Write.
func (o *outbox) queue(req *ReportRequest) error {
	return o.queueWith(req, nil)
}

// queueWith is queue with fn run in the same Write after the report was
// inserted (a run recorded as finished together with its report). When fn
// fails nothing is stored.
func (o *outbox) queueWith(req *ReportRequest, fn func(tx models.Writer) error) error {
	var over bool
	var dropped int
	err := o.db.Write(func(tx models.Writer) error {
		if err := o.insert(tx, req); err != nil {
			return err
		}
		if fn != nil {
			if err := fn(tx); err != nil {
				return err
			}
		}
		var err error
		over, dropped, err = o.applyCommandLimit(tx)
		return err
	})
	if err == nil && over {
		if dropped > 0 {
			o.logf("report storage limit reached: dropped %d command set report(s)", dropped)
		} else {
			o.logf("report storage limit reached: dropped the console output of queued command set results")
		}
	}
	return err
}

// queuePeriodic stores the report of a monitoring step, in one Write: the
// detections dropped earlier and not yet reported are put into req.Dropped
// (and the count reset), older rows with nothing else to deliver (no
// detections, no dropped count, nothing about command sets and the same
// application result as the new one) are deleted, the host information is
// removed from the other rows (only the newest periodic report carries it),
// the report is inserted, and the limit of queued detections is applied.
// It returns how many detections the limit dropped (counted for the next
// report).
func (o *outbox) queuePeriodic(req *ReportRequest) (int, error) {
	dropped := 0
	err := o.db.Write(func(tx models.Writer) error {
		dropped = 0
		pending, _ := strconv.Atoi(models.GetLocalState(tx, stateDroppedPending))
		req.Dropped = pending
		if err := pruneForPeriodic(tx, req); err != nil {
			return err
		}
		if err := o.insert(tx, req); err != nil {
			return err
		}
		if err := models.SetLocalState(tx, stateDroppedPending, "0"); err != nil {
			return err
		}
		var err error
		dropped, err = applyDetectionLimit(tx)
		return err
	})
	if err != nil {
		return 0, err
	}
	return dropped, nil
}

// flush sends the reports queued when it starts, in the order of their rows,
// one request each, and deletes a row (one Write) after the central answered
// it. It stops at the first failure, which it hands to onError; that row and
// the later ones stay queued and are sent again, with the same identity,
// next time. Each row is read again just before it is sent, so a row that
// a limit shrank meanwhile (command set reports are queued by other workers
// during a flush) is sent as it is now, and a row the limit deleted is not
// sent. A row that cannot be parsed is deleted. The rows are read on the
// read side; no request to the central is made inside a Write.
func (o *outbox) flush(tr AgentTransport, onError func(error)) {
	items, err := models.ListOutbox(o.db.Read())
	if err != nil {
		return
	}
	deleteItem := func(id int64) {
		_ = o.db.Write(func(tx models.Writer) error { return models.DeleteOutbox(tx, id) })
	}
	for _, listed := range items {
		it, err := models.GetOutbox(o.db.Read(), listed.ID)
		if err != nil {
			return
		}
		if it == nil {
			continue
		}
		var req ReportRequest
		if err := json.Unmarshal([]byte(it.Payload), &req); err != nil {
			deleteItem(it.ID)
			continue
		}
		if err := tr.Report(&req); err != nil {
			onError(err)
			return
		}
		deleteItem(it.ID)
	}
}

// insert gives req a new report identity and stores it as a new row, inside
// the write transaction tx.
func (o *outbox) insert(tx models.Writer, req *ReportRequest) error {
	id, err := RandomHex(16)
	if err != nil {
		return err
	}
	req.ReportID = id
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	a, n := countCategories(req.Detections)
	return models.InsertOutbox(tx, &models.OutboxItem{Payload: string(b), AlertCount: a, NormalCount: n,
		CommandBytes: commandBytesOf(req, len(b))})
}

// pruneForPeriodic prepares the queued rows for the periodic report req
// inside tx: it deletes the rows that have nothing else to deliver and
// removes the host information from the others.
func pruneForPeriodic(tx models.Writer, req *ReportRequest) error {
	apply, _ := json.Marshal(req.Apply)
	items, err := models.ListOutbox(tx)
	if err != nil {
		return err
	}
	for _, it := range items {
		var old ReportRequest
		if json.Unmarshal([]byte(it.Payload), &old) != nil {
			continue
		}
		oldApply, _ := json.Marshal(old.Apply)
		if len(old.Detections) == 0 && old.Dropped == 0 && string(oldApply) == string(apply) &&
			len(old.CommandRuns) == 0 && len(old.CommandRequestAcks) == 0 && old.CommandApply == nil {
			if err := models.DeleteOutbox(tx, it.ID); err != nil {
				return err
			}
			continue
		}
		if old.HostInfo != nil {
			old.HostInfo = nil
			ob, err := json.Marshal(&old)
			if err != nil {
				return err
			}
			it.Payload = string(ob)
			it.CommandBytes = commandBytesOf(&old, len(ob))
			if err := models.UpdateOutbox(tx, it); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyDetectionLimit keeps the queued detections under
// DefaultOutboxMaxDetection inside tx by dropping normal detections oldest
// first (then alerts oldest first if still needed), and adds what it dropped
// to the count reported with the next report. It returns that number.
func applyDetectionLimit(tx models.Writer) (int, error) {
	total := models.OutboxDetectionCount(tx)
	if total <= DefaultOutboxMaxDetection {
		return 0, nil
	}
	excess := total - DefaultOutboxMaxDetection
	items, err := models.ListOutbox(tx)
	if err != nil {
		return 0, err
	}
	dropped := 0
	for _, pass := range []string{CategoryNormal, CategoryAlert} {
		for _, it := range items {
			if excess <= 0 {
				break
			}
			var req ReportRequest
			if json.Unmarshal([]byte(it.Payload), &req) != nil {
				continue
			}
			kept := req.Detections[:0]
			for _, d := range req.Detections {
				isPass := (pass == CategoryAlert && d.Category == CategoryAlert) || (pass == CategoryNormal && d.Category != CategoryAlert)
				if excess > 0 && isPass {
					excess--
					dropped++
					continue
				}
				kept = append(kept, d)
			}
			req.Detections = kept
			b, _ := json.Marshal(&req)
			it.Payload = string(b)
			it.CommandBytes = commandBytesOf(&req, len(b))
			it.AlertCount, it.NormalCount = countCategories(kept)
			if err := models.UpdateOutbox(tx, it); err != nil {
				return 0, err
			}
		}
	}
	if dropped == 0 {
		return 0, nil
	}
	prev, _ := strconv.Atoi(models.GetLocalState(tx, stateDroppedPending))
	if err := models.SetLocalState(tx, stateDroppedPending, strconv.Itoa(prev+dropped)); err != nil {
		return 0, err
	}
	return dropped, nil
}

// applyCommandLimit keeps the queued command set reports under o.limit
// inside tx. Oldest first, it drops the console output of run_only sets,
// then of collect_output sets (their values are kept, the results are
// marked truncated), then the reports of commands that do not close a run,
// and last the closing reports. over tells whether the limit was exceeded,
// dropped how many run reports were dropped.
func (o *outbox) applyCommandLimit(tx models.Writer) (over bool, dropped int, err error) {
	limit := o.limit
	total := models.OutboxCommandBytes(tx)
	if total <= limit {
		return false, 0, nil
	}
	items, err := models.ListOutbox(tx)
	if err != nil {
		return false, 0, err
	}
	for pass := 0; pass < 4 && total > limit; pass++ {
		for _, it := range items {
			if total <= limit {
				break
			}
			if it.CommandBytes == 0 {
				continue
			}
			var req ReportRequest
			if json.Unmarshal([]byte(it.Payload), &req) != nil {
				continue
			}
			changed := false
			kept := req.CommandRuns[:0]
			for _, run := range req.CommandRuns {
				switch pass {
				case 0, 1:
					if (pass == 0) != (run.Purpose == PurposeRunOnly) {
						break
					}
					for j := range run.Results {
						if len(run.Results[j].Output) > 0 {
							run.Results[j].Output = nil
							run.Results[j].Truncated = true
							changed = true
						}
					}
				case 2, 3:
					if (pass == 3) == run.Finished {
						changed = true
						dropped++
						continue
					}
				}
				kept = append(kept, run)
			}
			req.CommandRuns = kept
			if !changed {
				continue
			}
			if len(req.CommandRuns) == 0 && len(req.Detections) == 0 && len(req.CommandRequestAcks) == 0 &&
				req.CommandApply == nil && req.Dropped == 0 && req.HostInfo == nil && req.Apply == nil {
				if err := models.DeleteOutbox(tx, it.ID); err != nil {
					return false, 0, err
				}
				total -= it.CommandBytes
				it.CommandBytes = 0
				continue
			}
			b, err := json.Marshal(&req)
			if err != nil {
				continue
			}
			nb := commandBytesOf(&req, len(b))
			total -= it.CommandBytes - nb
			it.Payload, it.CommandBytes = string(b), nb
			if err := models.UpdateOutbox(tx, it); err != nil {
				return false, 0, err
			}
		}
	}
	return true, dropped, nil
}

// commandBytesOf is the size counted against the outbox limit of command set
// reports: the payload size when the report carries runs.
func commandBytesOf(req *ReportRequest, size int) int64 {
	if len(req.CommandRuns) == 0 {
		return 0
	}
	return int64(size)
}

func countCategories(ds []DetectionReport) (alerts, normals int) {
	for _, d := range ds {
		if d.Category == CategoryAlert {
			alerts++
		} else {
			normals++
		}
	}
	return
}
