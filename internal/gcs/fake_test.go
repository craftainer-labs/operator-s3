package gcs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
)

// fakeGCS is the bucket calls of the Cloud Storage JSON API for one project,
// in memory. It records the calls as "METHOD /path".
type fakeGCS struct {
	mu      sync.Mutex
	buckets map[string]map[string]any
	calls   []string
	// projects are the project query parameters of the create and list calls.
	projects []string
	// objects are the buckets that hold an object.
	objects map[string]bool
	// refuse maps "METHOD" (or "METHOD list") to the status answered instead of
	// the normal one.
	refuse map[string]int
}

func newFakeGCS() *fakeGCS {
	return &fakeGCS{buckets: map[string]map[string]any{}, objects: map[string]bool{}, refuse: map[string]int{}}
}

// serve starts the fake and returns the client of a Store that reaches it.
func (f *fakeGCS) serve(t *testing.T) *cloud.GCPClient {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &cloud.GCPClient{HTTPClient: srv.Client(), Endpoint: srv.URL + "/"}
}

func (f *fakeGCS) callList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

func (f *fakeGCS) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeGCS) lastProject() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.projects[len(f.projects)-1]
}

func (f *fakeGCS) setLabel(bucket, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buckets[bucket]["labels"].(map[string]any)[key] = value
}

func (f *fakeGCS) bucket(name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buckets[name]
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func problem(w http.ResponseWriter, status int, reason string) {
	reply(w, status, map[string]any{"error": map[string]any{
		"code": status, "message": "secret detail 127.0.0.1",
		"errors": []map[string]any{{"reason": reason, "message": "secret detail 127.0.0.1"}},
	}})
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	rest, ok := strings.CutPrefix(r.URL.Path, "/storage/v1/b")
	if !ok {
		problem(w, http.StatusNotFound, "notFound")
		return
	}
	name := strings.Trim(rest, "/")
	key := r.Method
	if name == "" && r.Method == http.MethodGet {
		key += " list"
	}
	if status := f.refuse[key]; status != 0 {
		problem(w, status, "boom")
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch {
	case name == "" && r.Method == http.MethodGet:
		f.projects = append(f.projects, r.URL.Query().Get("project"))
		items := []any{}
		for n := range f.buckets {
			items = append(items, map[string]any{"name": n})
			break
		}
		reply(w, http.StatusOK, map[string]any{"kind": "storage#buckets", "items": items})
	case name == "" && r.Method == http.MethodPost:
		f.create(w, r, body)
	case f.buckets[name] == nil:
		problem(w, http.StatusNotFound, "notFound")
	case r.Method == http.MethodGet:
		reply(w, http.StatusOK, f.buckets[name])
	case r.Method == http.MethodPatch:
		f.patch(w, name, body)
	case r.Method == http.MethodDelete && f.objects[name]:
		problem(w, http.StatusConflict, "conflict")
	case r.Method == http.MethodDelete:
		delete(f.buckets, name)
		reply(w, http.StatusNoContent, nil)
	default:
		problem(w, http.StatusMethodNotAllowed, "notAllowed")
	}
}

func (f *fakeGCS) create(w http.ResponseWriter, r *http.Request, body []byte) {
	f.projects = append(f.projects, r.URL.Query().Get("project"))
	var b map[string]any
	if err := json.Unmarshal(body, &b); err != nil {
		problem(w, http.StatusBadRequest, "invalid")
		return
	}
	name, _ := b["name"].(string)
	if f.buckets[name] != nil {
		problem(w, http.StatusConflict, "conflict")
		return
	}
	f.buckets[name] = b
	reply(w, http.StatusOK, b)
}

// patch applies a JSON merge: labels merge key by key (null removes one), any
// other field is replaced.
func (f *fakeGCS) patch(w http.ResponseWriter, name string, body []byte) {
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		problem(w, http.StatusBadRequest, "invalid")
		return
	}
	b := f.buckets[name]
	for k, v := range in {
		if k != "labels" {
			b[k] = v
			continue
		}
		labels, _ := b["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		for lk, lv := range v.(map[string]any) {
			if lv == nil {
				delete(labels, lk)
			} else {
				labels[lk] = lv
			}
		}
		b["labels"] = labels
	}
	reply(w, http.StatusOK, b)
}

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}
