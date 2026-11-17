package awss3

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// fakeBucket is one bucket of the fake.
type fakeBucket struct {
	tags       map[string]string
	versioning string
	rules      []fakeRule
	hasRules   bool
	objects    int
	lock       bool
	location   string
}

type fakeRule struct {
	XMLName xml.Name `xml:"Rule"`
	ID      string   `xml:"ID"`
	Status  string   `xml:"Status"`
	Prefix  string   `xml:"Filter>Prefix"`
	Days    int32    `xml:"Expiration>Days"`
}

// refusal is an error answer injected for a request.
type refusal struct {
	status int
	code   string
}

// fakeS3 is a path-style S3 and STS of one account, in memory. It answers the
// calls of the Store and records them as "METHOD /bucket?subresource".
type fakeS3 struct {
	mu      sync.Mutex
	buckets map[string]*fakeBucket
	calls   []string
	// refuse maps "METHOD subresource" (for example "GET tagging", "PUT",
	// "HEAD") to the answer given to it instead of the normal one.
	refuse map[string]refusal
	// rawLifecycle, when set, is the body of GET lifecycle.
	rawLifecycle string
	// sts is the status of GetCallerIdentity.
	sts int
}

func newFakeS3() *fakeS3 {
	return &fakeS3{buckets: map[string]*fakeBucket{}, refuse: map[string]refusal{}, sts: http.StatusOK}
}

// serve starts the fake and returns an aws.Config that reaches it.
func (f *fakeS3) serve(t *testing.T, region string) aws.Config {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return testConfig(srv.URL, region, srv.Client())
}

func (f *fakeS3) callList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

func (f *fakeS3) bucket(name string) *fakeBucket {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buckets[name]
}

func (f *fakeS3) put(name string, b *fakeBucket) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buckets[name] = b
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>secret detail</Message></Error>`, code)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Path == "/" { // STS
		f.calls = append(f.calls, "STS")
		if f.sts != http.StatusOK {
			writeError(w, f.sts, "InvalidClientTokenId")
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:iam::123456789012:user/x</Arn><UserId>U</UserId><Account>123456789012</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`)
		return
	}
	name := strings.Trim(r.URL.Path, "/")
	sub := ""
	for _, k := range []string{"tagging", "versioning", "lifecycle"} {
		if r.URL.Query().Has(k) {
			sub = k
		}
	}
	f.calls = append(f.calls, strings.TrimSpace(r.Method+" /"+name+"?"+sub))
	if ref, ok := f.refuse[strings.TrimSpace(r.Method+" "+sub)]; ok {
		writeError(w, ref.status, ref.code)
		return
	}
	if name == "" { // ListBuckets
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<ListAllMyBucketsResult><Owner><ID>o</ID></Owner><Buckets></Buckets></ListAllMyBucketsResult>`)
		return
	}
	b := f.buckets[name]
	if r.Method == http.MethodPut && sub == "" {
		f.create(w, r, name, body)
		return
	}
	if b == nil {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeError(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	f.sub(w, r, name, sub, b, body)
}

func (f *fakeS3) create(w http.ResponseWriter, r *http.Request, name string, body []byte) {
	if f.buckets[name] != nil {
		writeError(w, http.StatusConflict, "BucketAlreadyOwnedByYou")
		return
	}
	b := &fakeBucket{tags: map[string]string{}, lock: r.Header.Get("X-Amz-Bucket-Object-Lock-Enabled") == "true"}
	var cfg struct {
		Location string `xml:"LocationConstraint"`
	}
	_ = xml.Unmarshal(body, &cfg)
	b.location = cfg.Location
	f.buckets[name] = b
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) sub(w http.ResponseWriter, r *http.Request, name, sub string, b *fakeBucket, body []byte) {
	switch r.Method + " " + sub {
	case "HEAD ":
		w.WriteHeader(http.StatusOK)
	case "DELETE ":
		if b.objects > 0 {
			writeError(w, http.StatusConflict, "BucketNotEmpty")
			return
		}
		delete(f.buckets, name)
		w.WriteHeader(http.StatusNoContent)
	case "GET tagging":
		if len(b.tags) == 0 {
			writeError(w, http.StatusNotFound, "NoSuchTagSet")
			return
		}
		var sb strings.Builder
		sb.WriteString(`<Tagging><TagSet>`)
		for k, v := range b.tags {
			fmt.Fprintf(&sb, `<Tag><Key>%s</Key><Value>%s</Value></Tag>`, k, v)
		}
		sb.WriteString(`</TagSet></Tagging>`)
		_, _ = io.WriteString(w, sb.String())
	case "PUT tagging":
		var t struct {
			Tags []struct{ Key, Value string } `xml:"TagSet>Tag"`
		}
		_ = xml.Unmarshal(body, &t)
		b.tags = map[string]string{}
		for _, tag := range t.Tags {
			b.tags[tag.Key] = tag.Value
		}
	case "GET versioning":
		_, _ = fmt.Fprintf(w, `<VersioningConfiguration><Status>%s</Status></VersioningConfiguration>`, b.versioning)
	case "PUT versioning":
		var v struct {
			Status string `xml:"Status"`
		}
		_ = xml.Unmarshal(body, &v)
		b.versioning = v.Status
	case "GET lifecycle":
		if f.rawLifecycle != "" {
			_, _ = io.WriteString(w, f.rawLifecycle)
			return
		}
		if !b.hasRules {
			writeError(w, http.StatusNotFound, "NoSuchLifecycleConfiguration")
			return
		}
		_, _ = io.WriteString(w, `<LifecycleConfiguration>`)
		for _, rule := range b.rules {
			out, _ := xml.Marshal(rule)
			_, _ = w.Write(out)
		}
		_, _ = io.WriteString(w, `</LifecycleConfiguration>`)
	case "PUT lifecycle":
		var c struct {
			Rules []fakeRule `xml:"Rule"`
		}
		_ = xml.Unmarshal(body, &c)
		b.rules, b.hasRules = c.Rules, true
	case "DELETE lifecycle":
		b.rules, b.hasRules = nil, false
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusNotImplemented, "NotImplemented")
	}
}
