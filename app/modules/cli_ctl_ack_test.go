package modules

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ackTestClient serves the two routes ctl detections ack uses for the
// detections in known ("agent:id") and records the acknowledge requests.
func ackTestClient(t *testing.T, known map[string]bool, acked *[]string) *ctlClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/v1/detections/{agent}/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !known[r.PathValue("agent")+":"+r.PathValue("id")] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"detection not found"}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("POST /admin/v1/detections/ack", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Items []struct {
				AgentID int64 `json:"agent_id"`
				ID      int64 `json:"id"`
			} `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, it := range in.Items {
			*acked = append(*acked, fmt.Sprintf("%d:%d", it.AgentID, it.ID))
		}
		_, _ = fmt.Fprintf(w, `{"acknowledged":%d}`, len(in.Items))
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	tlsCfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsCfg.ServerName = ctlServerName
	hc := &HTTPSClient{Address: srv.Listener.Addr().String(), TLS: tlsCfg, Headers: map[string]string{}}
	return &ctlClient{http: hc, json: true}
}

// ctl detections ack acknowledges nothing when any given detection does not
// exist, names the missing ones and ends with the exit code of a missing
// object; otherwise it acknowledges them all.
func TestCtlAckDetectionsMissing(t *testing.T) {
	var acked []string
	cl := ackTestClient(t, map[string]bool{"2:4": true, "2:5": true}, &acked)

	err := cl.ackDetections([]map[string]int64{{"agent_id": 2, "id": 4}, {"agent_id": 2, "id": 99}, {"agent_id": 99, "id": 1}})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitArgument {
		t.Fatalf("missing detections: %v", err)
	}
	if !strings.Contains(ee.Message, "2:99, 99:1") || strings.Contains(ee.Message, "2:4") || !strings.Contains(ee.Message, "nothing was acknowledged") {
		t.Fatalf("message: %q", ee.Message)
	}
	if len(acked) != 0 {
		t.Fatalf("acknowledged although one is missing: %v", acked)
	}

	if err := cl.ackDetections([]map[string]int64{{"agent_id": 2, "id": 4}, {"agent_id": 2, "id": 5}}); err != nil {
		t.Fatalf("existing detections: %v", err)
	}
	if strings.Join(acked, ",") != "2:4,2:5" {
		t.Fatalf("acknowledged: %v", acked)
	}
}
