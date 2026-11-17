package awss3

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// fakeKey is an access key of the fake.
type fakeKey struct{ id, secret string }

// fakeUser is an IAM user of the fake.
type fakeUser struct {
	path, boundary string
	tags           map[string]string
	policies       map[string]string
	keys           []fakeKey
}

// fakeIAM is IAM of one account, in memory, behind the query protocol. It
// records the actions it receives, with the user they name.
type fakeIAM struct {
	mu    sync.Mutex
	users map[string]*fakeUser
	calls []string
	// refuse maps an action (for example "CreateUser") to the answer given to
	// it instead of the normal one.
	refuse map[string]refusal
	seq    int
}

func newFakeIAM() *fakeIAM {
	return &fakeIAM{users: map[string]*fakeUser{}, refuse: map[string]refusal{}}
}

// serve starts the fake and returns an aws.Config that reaches it.
func (f *fakeIAM) serve(t *testing.T) aws.Config {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return testConfig(srv.URL, "us-east-1", srv.Client())
}

func (f *fakeIAM) callList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

func (f *fakeIAM) user(name string) *fakeUser {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[name]
}

func (f *fakeIAM) put(name string, u *fakeUser) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[name] = u
}

func iamError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<ErrorResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/"><Error><Type>Sender</Type><Code>%s</Code><Message>secret detail</Message></Error><RequestId>r</RequestId></ErrorResponse>`, code)
}

// iamAnswer writes the answer of an action with the given result element.
func iamAnswer(w http.ResponseWriter, action, result string) {
	w.Header().Set("Content-Type", "text/xml")
	res := ""
	if result != "" {
		res = fmt.Sprintf("<%sResult>%s</%[1]sResult>", action, result)
	}
	_, _ = fmt.Fprintf(w, `<%sResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/">%s<ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></%[1]sResponse>`, action, res)
}

func (u *fakeUser) xml(name string) string {
	var tags strings.Builder
	for _, k := range slices.Sorted(maps.Keys(u.tags)) {
		fmt.Fprintf(&tags, `<member><Key>%s</Key><Value>%s</Value></member>`, k, u.tags[k])
	}
	return fmt.Sprintf(`<User><Path>%s</Path><UserName>%s</UserName><UserId>U</UserId><Arn>arn:aws:iam::123456789012:user%s%s</Arn><Tags>%s</Tags></User>`,
		u.path, name, u.path, name, tags.String())
}

func (f *fakeIAM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	action, name := r.Form.Get("Action"), r.Form.Get("UserName")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.TrimSpace(action+" "+name+" "+r.Form.Get("PolicyName")+r.Form.Get("AccessKeyId")))
	if ref, ok := f.refuse[action]; ok {
		iamError(w, ref.status, ref.code)
		return
	}
	u := f.users[name]
	if action == "CreateUser" {
		f.create(w, r, name)
		return
	}
	if u == nil {
		iamError(w, http.StatusNotFound, "NoSuchEntity")
		return
	}
	switch action {
	case "GetUser":
		iamAnswer(w, action, u.xml(name))
	case "ListUserPolicies":
		var sb strings.Builder
		for _, p := range slices.Sorted(maps.Keys(u.policies)) {
			fmt.Fprintf(&sb, `<member>%s</member>`, p)
		}
		iamAnswer(w, action, `<PolicyNames>`+sb.String()+`</PolicyNames><IsTruncated>false</IsTruncated>`)
	case "PutUserPolicy":
		u.policies[r.Form.Get("PolicyName")] = r.Form.Get("PolicyDocument")
		iamAnswer(w, action, "")
	case "DeleteUserPolicy":
		if _, ok := u.policies[r.Form.Get("PolicyName")]; !ok {
			iamError(w, http.StatusNotFound, "NoSuchEntity")
			return
		}
		delete(u.policies, r.Form.Get("PolicyName"))
		iamAnswer(w, action, "")
	default:
		f.keyAction(w, r, action, name, u)
	}
}

func (f *fakeIAM) create(w http.ResponseWriter, r *http.Request, name string) {
	if f.users[name] != nil {
		iamError(w, http.StatusConflict, "EntityAlreadyExists")
		return
	}
	u := &fakeUser{
		path: r.Form.Get("Path"), boundary: r.Form.Get("PermissionsBoundary"),
		tags: map[string]string{}, policies: map[string]string{},
	}
	if u.path == "" {
		u.path = "/"
	}
	for i := 1; r.Form.Has(fmt.Sprintf("Tags.member.%d.Key", i)); i++ {
		u.tags[r.Form.Get(fmt.Sprintf("Tags.member.%d.Key", i))] = r.Form.Get(fmt.Sprintf("Tags.member.%d.Value", i))
	}
	f.users[name] = u
	iamAnswer(w, "CreateUser", u.xml(name))
}

func (f *fakeIAM) keyAction(w http.ResponseWriter, r *http.Request, action, name string, u *fakeUser) {
	switch action {
	case "ListAccessKeys":
		var sb strings.Builder
		for _, k := range u.keys {
			fmt.Fprintf(&sb, `<member><UserName>%s</UserName><AccessKeyId>%s</AccessKeyId><Status>Active</Status></member>`, name, k.id)
		}
		iamAnswer(w, action, `<AccessKeyMetadata>`+sb.String()+`</AccessKeyMetadata><IsTruncated>false</IsTruncated>`)
	case "CreateAccessKey":
		if len(u.keys) >= 2 {
			iamError(w, http.StatusConflict, "LimitExceeded")
			return
		}
		f.seq++
		k := fakeKey{id: fmt.Sprintf("AKIAFAKE%012d", f.seq), secret: fmt.Sprintf("secret-%d", f.seq)}
		u.keys = append(u.keys, k)
		iamAnswer(w, action, fmt.Sprintf(`<AccessKey><UserName>%s</UserName><AccessKeyId>%s</AccessKeyId><Status>Active</Status><SecretAccessKey>%s</SecretAccessKey></AccessKey>`, name, k.id, k.secret))
	case "DeleteAccessKey":
		id := r.Form.Get("AccessKeyId")
		i := slices.IndexFunc(u.keys, func(k fakeKey) bool { return k.id == id })
		if i < 0 {
			iamError(w, http.StatusNotFound, "NoSuchEntity")
			return
		}
		u.keys = slices.Delete(u.keys, i, i+1)
		iamAnswer(w, action, "")
	case "DeleteUser":
		if len(u.keys) > 0 || len(u.policies) > 0 {
			iamError(w, http.StatusConflict, "DeleteConflict")
			return
		}
		delete(f.users, name)
		iamAnswer(w, action, "")
	default:
		iamError(w, http.StatusNotImplemented, "NotImplemented")
	}
}
