package s3client

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore is a RustFS: the admin API and the S3 bucket API over an HTTPS
// test server, in memory. It checks the SigV4 signature of every request with
// its own implementation of the algorithm, and records what it was asked. There is no
// real RustFS to run against until NO3b-2, so its routes follow the admin
// handlers of rustfs/rustfs at 1.0.0-rc.5.
type fakeStore struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	reqs     []string
	bucket   map[string]*fakeBucket
	user     map[string]*fakeUser
	policy   map[string]string
	key      map[string]*fakeKey
	quota    map[string]int64
	deleted  []string
	failOn   string
	statusOn map[string]int
	body     map[string]string
	// lenient makes a bad signature a 403 without a test failure.
	lenient bool
	// quotaType is what the last quota write carried.
	quotaType string
	// special answers a request instead of the routes when it returns true.
	special func(w http.ResponseWriter, r *http.Request) bool
}

type fakeBucket struct {
	tags       map[string]string
	versioning string
	lifecycle  string
	lock       bool
	objects    bool
}

type fakeUser struct {
	status   string
	policies []string
}

type fakeKey struct{ parent, secret string }

const (
	testAccess = "admin-ak"
	testSecret = "admin-sk"
)

func newFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	f := &fakeStore{
		t: t, bucket: map[string]*fakeBucket{}, user: map[string]*fakeUser{}, policy: map[string]string{},
		key: map[string]*fakeKey{}, quota: map[string]int64{}, statusOn: map[string]int{}, body: map[string]string{},
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// target is a Target for the fake that trusts its certificate and dials
// without an egress policy.
func (f *fakeStore) target() Target {
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	return Target{
		Endpoint: f.srv.URL, AccessKey: testAccess, SecretKey: testSecret, CABundle: ca,
		Dial: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	}
}

func (f *fakeStore) client() *Client {
	f.t.Helper()
	c, err := New(f.target())
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// requests returns what was asked, as "METHOD /path?query".
func (f *fakeStore) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *fakeStore) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = nil
}

// asked reports whether a request line contains all of subs.
func (f *fakeStore) asked(subs ...string) bool {
	for _, r := range f.requests() {
		ok := true
		for _, s := range subs {
			ok = ok && strings.Contains(r, s)
		}
		if ok {
			return true
		}
	}
	return false
}

func (f *fakeStore) setBody(match, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body[match] = body
}

func xmlError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>secret detail</Message></Error>", code)
}

func hmacSHA(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// verify checks the SigV4 signature of a request against the admin
// credentials, the region us-east-1 and the service s3, from the headers the
// request says it signed.
func (f *fakeStore) verify(r *http.Request) string {
	rest, ok := strings.CutPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ")
	if !ok {
		return "no SigV4 authorization"
	}
	fields := map[string]string{}
	for _, kv := range strings.Split(rest, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		fields[k] = v
	}
	scope, ok := strings.CutPrefix(fields["Credential"], testAccess+"/")
	parts := strings.Split(scope, "/")
	if !ok || len(parts) != 4 || parts[1] != "us-east-1" || parts[2] != "s3" || parts[3] != "aws4_request" {
		return "credential scope " + fields["Credential"]
	}
	var canon strings.Builder
	for _, name := range strings.Split(fields["SignedHeaders"], ";") {
		v := r.Header.Get(name)
		if name == "host" {
			v = r.Host
		}
		canon.WriteString(name + ":" + strings.Join(strings.Fields(v), " ") + "\n")
	}
	query := strings.ReplaceAll(r.URL.Query().Encode(), "+", "%20")
	request := strings.Join([]string{
		r.Method, r.URL.EscapedPath(), query, canon.String(), fields["SignedHeaders"], r.Header.Get("X-Amz-Content-Sha256"),
	}, "\n")
	sum := sha256.Sum256([]byte(request))
	toSign := "AWS4-HMAC-SHA256\n" + r.Header.Get("X-Amz-Date") + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := hmacSHA([]byte("AWS4"+testSecret), parts[0])
	for _, p := range parts[1:] {
		key = hmacSHA(key, p)
	}
	if hex.EncodeToString(hmacSHA(key, toSign)) != fields["Signature"] {
		return "signature mismatch for " + r.Method + " " + r.URL.String()
	}
	return ""
}

func (f *fakeStore) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	line := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, line)
	if f.special != nil && f.special(w, r) {
		return
	}
	if bad := f.verify(r); bad != "" {
		if !f.lenient {
			f.t.Errorf("request %s: %s", line, bad)
		}
		xmlError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	for match, status := range f.statusOn {
		if strings.Contains(line, match) {
			if status/100 == 3 {
				w.Header().Set("Location", "https://elsewhere.example.test/")
			}
			xmlError(w, status, "Injected")
			return
		}
	}
	if f.failOn != "" && strings.Contains(line, f.failOn) {
		xmlError(w, http.StatusInternalServerError, "InternalError")
		return
	}
	for match, out := range f.body {
		if strings.Contains(line, match) {
			_, _ = io.WriteString(w, out)
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, adminPrefix+"/") {
		f.admin(w, r, strings.TrimPrefix(r.URL.Path, adminPrefix), body)
		return
	}
	f.s3(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/"), body)
}

func (f *fakeStore) admin(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	q := r.URL.Query()
	switch r.Method + " " + path {
	case "GET /user-info":
		u := f.user[q.Get("accessKey")]
		if u == nil {
			xmlError(w, http.StatusNotFound, "XAdminNoSuchUser")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": u.status, "policyName": strings.Join(u.policies, ",")})
	case "PUT /add-user":
		var in struct{ SecretKey, Status string }
		_ = json.Unmarshal(body, &in)
		f.user[q.Get("accessKey")] = &fakeUser{status: in.Status}
	case "PUT /set-user-status":
		f.user[q.Get("accessKey")].status = q.Get("status")
	case "DELETE /remove-user":
		if f.user[q.Get("accessKey")] == nil {
			xmlError(w, http.StatusNotFound, "XAdminNoSuchUser")
			return
		}
		delete(f.user, q.Get("accessKey"))
		f.deleted = append(f.deleted, "user:"+q.Get("accessKey"))
	case "PUT /add-canned-policy":
		f.policy[q.Get("name")] = string(body)
	case "DELETE /remove-canned-policy":
		if _, ok := f.policy[q.Get("name")]; !ok {
			xmlError(w, http.StatusNotFound, "XAdminNoSuchPolicy")
			return
		}
		delete(f.policy, q.Get("name"))
		f.deleted = append(f.deleted, "policy:"+q.Get("name"))
	case "POST /idp/builtin/policy/attach", "POST /idp/builtin/policy/detach":
		var in struct {
			Policies []string `json:"policies"`
			User     string   `json:"user"`
		}
		_ = json.Unmarshal(body, &in)
		u := f.user[in.User]
		if strings.HasSuffix(path, "attach") {
			u.policies = append(u.policies, in.Policies...)
			break
		}
		for _, p := range in.Policies {
			for i, have := range u.policies {
				if have == p {
					u.policies = append(u.policies[:i], u.policies[i+1:]...)
					break
				}
			}
		}
	case "GET /info-service-account":
		k := f.key[q.Get("accessKey")]
		if k == nil {
			xmlError(w, http.StatusNotFound, "XAdminNoSuchServiceAccount")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"parentUser": k.parent, "accessKey": q.Get("accessKey")})
	case "PUT /add-service-accounts":
		var in struct{ TargetUser, AccessKey, SecretKey string }
		_ = json.Unmarshal(body, &in)
		f.key[in.AccessKey] = &fakeKey{parent: in.TargetUser, secret: in.SecretKey}
		_, _ = fmt.Fprintf(w, `{"credentials":{"accessKey":%q,"secretKey":%q}}`, in.AccessKey, in.SecretKey)
	case "POST /update-service-account":
		var in struct{ NewSecretKey string }
		_ = json.Unmarshal(body, &in)
		f.key[q.Get("accessKey")].secret = in.NewSecretKey
		w.WriteHeader(http.StatusNoContent)
	case "DELETE /delete-service-account":
		delete(f.key, q.Get("accessKey"))
		f.deleted = append(f.deleted, "key:"+q.Get("accessKey"))
	case "GET /list-canned-policies":
		all := map[string]json.RawMessage{}
		for name, doc := range f.policy {
			if json.Valid([]byte(doc)) {
				all[name] = json.RawMessage(doc)
			} else {
				all[name] = json.RawMessage("{}")
			}
		}
		_ = json.NewEncoder(w).Encode(all)
	case "GET /get-bucket-quota":
		_ = json.NewEncoder(w).Encode(map[string]any{"quota": f.quota[q.Get("bucket")], "size": 0})
	case "PUT /set-bucket-quota":
		var in struct {
			Quota     int64
			Quotatype string
		}
		_ = json.Unmarshal(body, &in)
		f.quota[q.Get("bucket")] = in.Quota
		f.quotaType = in.Quotatype
	default:
		if strings.HasPrefix(path, "/quota/") && r.Method == http.MethodDelete {
			delete(f.quota, strings.TrimPrefix(path, "/quota/"))
			return
		}
		xmlError(w, http.StatusNotImplemented, "NotImplemented")
	}
}

func (f *fakeStore) s3(w http.ResponseWriter, r *http.Request, name string, body []byte) {
	b := f.bucket[name]
	resource := ""
	for _, k := range []string{"tagging", "versioning", "lifecycle"} {
		if r.URL.Query().Has(k) {
			resource = k
		}
	}
	if r.Method == http.MethodPut && resource == "" {
		f.bucket[name] = &fakeBucket{tags: map[string]string{}, lock: r.Header.Get("X-Amz-Bucket-Object-Lock-Enabled") == "true"}
		return
	}
	if b == nil {
		xmlError(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch r.Method + " " + resource {
	case "HEAD ":
	case "DELETE ":
		if b.objects {
			xmlError(w, http.StatusConflict, "BucketNotEmpty")
			return
		}
		delete(f.bucket, name)
		w.WriteHeader(http.StatusNoContent)
	case "GET tagging":
		if len(b.tags) == 0 {
			xmlError(w, http.StatusNotFound, "NoSuchTagSet")
			return
		}
		out := "<Tagging><TagSet>"
		for k, v := range b.tags {
			out += "<Tag><Key>" + k + "</Key><Value>" + v + "</Value></Tag>"
		}
		_, _ = io.WriteString(w, out+"</TagSet></Tagging>")
	case "PUT tagging":
		var in struct {
			Tags []struct{ Key, Value string } `xml:"TagSet>Tag"`
		}
		if err := xml.Unmarshal(body, &in); err != nil {
			f.t.Errorf("tagging body: %v", err)
		}
		b.tags = map[string]string{}
		for _, t := range in.Tags {
			b.tags[t.Key] = t.Value
		}
	case "GET versioning":
		_, _ = io.WriteString(w, "<VersioningConfiguration><Status>"+b.versioning+"</Status></VersioningConfiguration>")
	case "PUT versioning":
		var in struct{ Status string }
		if err := xml.Unmarshal(body, &in); err != nil {
			f.t.Errorf("versioning body: %v", err)
		}
		b.versioning = in.Status
	case "GET lifecycle":
		if b.lifecycle == "" {
			xmlError(w, http.StatusNotFound, "NoSuchLifecycleConfiguration")
			return
		}
		_, _ = io.WriteString(w, b.lifecycle)
	case "PUT lifecycle":
		b.lifecycle = string(body)
	case "DELETE lifecycle":
		b.lifecycle = ""
		w.WriteHeader(http.StatusNoContent)
	default:
		xmlError(w, http.StatusNotImplemented, "NotImplemented")
	}
}

// plainServer is an HTTP (not TLS) test server's address.
func plainServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return srv.URL
}
