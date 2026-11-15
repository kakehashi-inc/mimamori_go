package modules

import (
	"encoding/json"
	"slices"
	"strconv"

	"mimamori/app/models"
)

// The outbox of the agent: the items that wait for the central, in the table
// outbox of the agent's local database, and the batch being sent, in the
// state key outbox_batch. It is the only owner of both: every change goes
// through its methods, each one Write. Reports about command sets are
// queued by queue (from any flow), the report of a monitoring step by
// queuePeriodic; only the communication flow calls flush (its rounds and the
// last report at the stop), never twice at the same time.
//
// flush sends one batch in one request (ReportBatch). The batch is formed in
// one Write: the oldest queued items whose payloads together fit in
// MaxReportBatchBytes (always at least one; none when nothing is queued),
// under a new identity, recorded with the ids of its items. While a batch is
// recorded, it is sent again with the same identity and its items as they
// are then; nothing joins it, and items queued meanwhile go with the next
// batch. After the central answered, the items of the batch are deleted and
// the record cleared in one Write. So the central, which remembers the last
// batch it stored, receives a batch it stored again only as the last one,
// and stores it once.
//
// The content of a queued row only shrinks: host information is removed
// from all but the newest periodic row, and the limits drop output, runs
// and detections or delete rows (also rows of the recorded batch). A row is
// never given more content.

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
	o.logLimit(err, over, dropped)
	return err
}

// queueBuilt is queueWith for a report that build makes inside the Write,
// from what it reads there (and with what it writes there); when build fails
// nothing is stored.
func (o *outbox) queueBuilt(build func(tx models.Writer) (*ReportRequest, error)) error {
	var over bool
	var dropped int
	err := o.db.Write(func(tx models.Writer) error {
		req, err := build(tx)
		if err != nil {
			return err
		}
		if err := o.insert(tx, req); err != nil {
			return err
		}
		over, dropped, err = o.applyCommandLimit(tx)
		return err
	})
	o.logLimit(err, over, dropped)
	return err
}

// logLimit logs what the limit of command set reports dropped.
func (o *outbox) logLimit(err error, over bool, dropped int) {
	if err != nil || !over {
		return
	}
	if dropped > 0 {
		o.logf("report storage limit reached: dropped %d command set report(s)", dropped)
	} else {
		o.logf("report storage limit reached: dropped the console output of queued command set results")
	}
}

// keepDetections keeps detections that a monitoring step made known, inside
// the transaction tx that stores what makes them known, until the periodic
// report takes them (queuePeriodic). Beyond DefaultOutboxMaxDetection kept
// detections the oldest normal ones, then the oldest alerts, are dropped
// and counted with the dropped count, as in the outbox.
func keepDetections(tx models.Writer, ds []DetectionReport) error {
	if len(ds) == 0 {
		return nil
	}
	var kept []DetectionReport
	loadJSONState(tx, statePendingDetections, &kept)
	kept = append(kept, ds...)
	dropped := 0
	for _, alerts := range []bool{false, true} {
		for i := 0; i < len(kept) && len(kept) > DefaultOutboxMaxDetection; {
			if (kept[i].Category == CategoryAlert) == alerts {
				kept = slices.Delete(kept, i, i+1)
				dropped++
				continue
			}
			i++
		}
	}
	if dropped > 0 {
		prev, _ := strconv.Atoi(models.GetLocalState(tx, stateDroppedPending))
		if err := models.SetLocalState(tx, stateDroppedPending, strconv.Itoa(prev+dropped)); err != nil {
			return err
		}
	}
	return saveJSONState(tx, statePendingDetections, kept)
}

// queuePeriodic stores the report of a monitoring step, in one Write: the
// kept detections (keepDetections) come before those of req and are no
// longer kept, the detections dropped earlier and not yet reported are put into
// req.Dropped (and the count reset), older rows with nothing else to deliver
// (no detections, no dropped count, nothing about command sets and the same
// application result as the new one) are deleted, the host information is
// removed from the other rows (only the newest periodic report carries it),
// the report is inserted, and the limit of queued detections is applied.
// It returns how many detections the limit dropped (counted for the next
// report). When it fails, nothing changes: the detections stay kept.
func (o *outbox) queuePeriodic(req *ReportRequest) (int, error) {
	dropped := 0
	given := req.Detections
	err := o.db.Write(func(tx models.Writer) error {
		dropped = 0
		pending, _ := strconv.Atoi(models.GetLocalState(tx, stateDroppedPending))
		req.Dropped = pending
		var kept []DetectionReport
		loadJSONState(tx, statePendingDetections, &kept)
		req.Detections = append(kept, given...)
		if err := pruneForPeriodic(tx, req); err != nil {
			return err
		}
		if err := o.insert(tx, req); err != nil {
			return err
		}
		if err := models.SetLocalState(tx, stateDroppedPending, "0"); err != nil {
			return err
		}
		if err := models.DeleteLocalState(tx, statePendingDetections); err != nil {
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

// outboxBatch is the record of the batch being sent (state key
// outbox_batch): its identity and the ids of its items, oldest first.
type outboxBatch struct {
	ReportID string  `json:"report_id"`
	Items    []int64 `json:"items"`
}

// flush sends the batch takeBatch returns in one request, with stopping as
// given (the last report before the agent ends). After the central
// answered, the items of the batch are deleted and its record cleared in one
// Write. A failed request is handed to onError; then, and when that Write
// fails, the batch stays recorded and is sent again, with the same identity,
// next time. No request to the central is made inside a Write.
func (o *outbox) flush(tr AgentTransport, stopping bool, onError func(error)) {
	batch, items, err := o.takeBatch()
	if err != nil {
		o.logf("report: forming the batch of the queued items failed (nothing was sent; the next round tries again): %v", err)
		return
	}
	batch.Stopping = stopping
	if err := tr.Report(batch); err != nil {
		onError(err)
		return
	}
	if err := o.db.Write(func(tx models.Writer) error {
		for _, id := range items {
			if err := models.DeleteOutbox(tx, id); err != nil {
				return err
			}
		}
		return models.DeleteLocalState(tx, stateOutboxBatch)
	}); err != nil {
		o.logf("report %s was stored by the central, but removing its items from the queue failed (it is sent again and stored once): %v",
			batch.ReportID, err)
	}
}

// takeBatch returns the batch to send and the ids of its items, in one
// Write: the recorded batch with the items it still has, as they are now;
// otherwise a new batch of the oldest items whose payloads together fit in
// MaxReportBatchBytes (at least one, none when nothing is queued), under a
// new identity, recorded before it is returned. An item that cannot be
// parsed is deleted and not sent.
func (o *outbox) takeBatch() (*ReportBatch, []int64, error) {
	var batch *ReportBatch
	var items []int64
	err := o.db.Write(func(tx models.Writer) error {
		reports := []ReportRequest{}
		items = []int64{}
		// add appends the item id to items and its report to reports; an
		// item that is gone is skipped, one that cannot be parsed deleted.
		add := func(id int64) error {
			it, err := models.GetOutbox(tx, id)
			if err != nil || it == nil {
				return err
			}
			var req ReportRequest
			if json.Unmarshal([]byte(it.Payload), &req) != nil {
				return models.DeleteOutbox(tx, id)
			}
			items, reports = append(items, id), append(reports, req)
			return nil
		}
		if v := models.GetLocalState(tx, stateOutboxBatch); v != "" {
			var rec outboxBatch
			if json.Unmarshal([]byte(v), &rec) == nil && rec.ReportID != "" {
				for _, id := range rec.Items {
					if err := add(id); err != nil {
						return err
					}
				}
				batch = &ReportBatch{ReportID: rec.ReportID, Reports: reports}
				return nil
			}
		}
		sizes, err := models.ListOutboxSizes(tx)
		if err != nil {
			return err
		}
		var total int64
		for _, s := range sizes {
			if len(items) > 0 && total+s.Bytes > MaxReportBatchBytes {
				break
			}
			n := len(items)
			if err := add(s.ID); err != nil {
				return err
			}
			if len(items) > n {
				total += s.Bytes
			}
		}
		id, err := RandomHex(16)
		if err != nil {
			return err
		}
		if err := saveJSONState(tx, stateOutboxBatch, &outboxBatch{ReportID: id, Items: items}); err != nil {
			return err
		}
		batch = &ReportBatch{ReportID: id, Reports: reports}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return batch, items, nil
}

// insert stores req as a new row, inside the write transaction tx.
func (o *outbox) insert(tx models.Writer, req *ReportRequest) error {
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
