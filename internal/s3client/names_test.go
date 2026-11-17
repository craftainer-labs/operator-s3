package s3client

import (
	"regexp"
	"strings"
	"testing"
)

// ADR 0002 section 10: a bucket carries the short hash (it has a marker), an
// IAM user the long one (it has none), and the limits hold for the longest
// namespace and claim names.
func TestDerivedNames(t *testing.T) {
	ns, name := strings.Repeat("n", 63), strings.Repeat("c", 63)
	bucket := BucketName(ns, name)
	if len(bucket) > 63 || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`).MatchString(bucket) {
		t.Errorf("bucket = %q", bucket)
	}
	if got := BucketName("team-a", "orders"); got != "team-a-orders-"+hashOf(got) || len(hashOf(got)) != 10 {
		t.Errorf("bucket = %q: want the short hash (10 characters)", got)
	}
	user := UserName(ns, name)
	if len(user) > maxUserLen {
		t.Errorf("user = %q", user)
	}
	if got := UserName("team-a", "orders-app"); len(hashOf(got)) != 16 {
		t.Errorf("user = %q: want the long hash (16 characters)", got)
	}
	doc := AccessPolicy("aws", "b", "", true)
	if len(PolicyName(user, doc)) > 63 {
		t.Errorf("policy name = %q", PolicyName(user, doc))
	}
	// Two claims whose joined names are equal still differ.
	if UserName("team-a", "db") == UserName("team", "a-db") {
		t.Error("user names collide")
	}
}

// An AWS IAM user holds the marker as a tag, so its name carries the short hash
// and may be as long as IAM allows.
func TestIAMUserName(t *testing.T) {
	long := IAMUserName(strings.Repeat("n", 63), strings.Repeat("c", 63))
	if len(long) > maxIAMUserLen {
		t.Errorf("user = %q", long)
	}
	if got := IAMUserName("team-a", "orders-app"); got != "team-a-orders-app-"+hashOf(got) || len(hashOf(got)) != 10 {
		t.Errorf("user = %q: want the short hash (10 characters)", got)
	}
	if IAMUserName("team-a", "db") == IAMUserName("team", "a-db") {
		t.Error("user names collide")
	}
}

func hashOf(name string) string { return name[strings.LastIndex(name, "-")+1:] }

func TestAccessKeyIsDeterministicPerGeneration(t *testing.T) {
	a0, a1 := AccessKey("uid-1", 0), AccessKey("uid-1", 1)
	if a0 == a1 || a0 != AccessKey("uid-1", 0) || a0 == AccessKey("uid-2", 0) {
		t.Errorf("keys: %s %s", a0, a1)
	}
	if !regexp.MustCompile(`^[A-Z2-7]{20}$`).MatchString(a0) {
		t.Errorf("key = %q", a0)
	}
}

func TestPartition(t *testing.T) {
	for region, want := range map[string]string{
		"us-east-1": "aws", "eu-west-1": "aws", "": "aws", "cn-north-1": "aws-cn", "cn-northwest-1": "aws-cn",
		"us-gov-west-1": "aws-us-gov", "us-gov-east-1": "aws-us-gov",
		"us-iso-east-1": "aws-iso", "us-isob-east-1": "aws-iso-b", "eu-isoe-west-1": "aws-iso-e", "us-isof-south-1": "aws-iso-f",
	} {
		if got := Partition(region); got != want {
			t.Errorf("Partition(%q) = %q, want %q", region, got, want)
		}
	}
	for partition, arn := range map[string]string{"aws-cn": `"arn:aws-cn:s3:::bkt/*"`, "aws-us-gov": `"arn:aws-us-gov:s3:::bkt"`} {
		doc := string(AccessPolicy(partition, "bkt", "", true))
		if !strings.Contains(doc, arn) || strings.Contains(doc, "arn:aws:") {
			t.Errorf("%s policy: %s", partition, doc)
		}
	}
}

func TestAccessPolicy(t *testing.T) {
	rw := string(AccessPolicy("aws", "bkt", "app/", true))
	ro := string(AccessPolicy("aws", "bkt", "app/", false))
	whole := string(AccessPolicy("aws", "bkt", "", false))
	for _, want := range []string{`"arn:aws:s3:::bkt/app/*"`, `"s3:PutObject"`, `"s3:DeleteObject"`, `"s3:ListBucketMultipartUploads"`,
		`"StringLike":{"s3:prefix":["app/*"]}`, `"s3:GetBucketLocation"`} {
		if !strings.Contains(rw, want) {
			t.Errorf("read-write policy lacks %s: %s", want, rw)
		}
	}
	for _, deny := range []string{`s3:PutObject`, `s3:DeleteObject`} {
		if strings.Contains(ro, deny) {
			t.Errorf("read-only policy has %s", deny)
		}
	}
	if strings.Contains(whole, "Condition") || !strings.Contains(whole, `"arn:aws:s3:::bkt/*"`) {
		t.Errorf("whole-bucket policy: %s", whole)
	}
	// The name follows the document.
	again := PolicyName("u", []byte(rw))
	if PolicyName("u", []byte(rw)) == PolicyName("u", []byte(ro)) || again != PolicyName("u", []byte(rw)) {
		t.Error("policy names")
	}
	if OwnsPolicy("u", "u-extra") || OwnsPolicy("u", "u-v-0123abcd") || !OwnsPolicy("u-v", "u-v-0123abcd") {
		t.Error("ownership is the exact shape user-<8 hex>")
	}
	if !OwnsPolicy("u", PolicyName("u", []byte(rw))) || OwnsPolicy("u", "readwrite") || OwnsPolicy("u", PolicyName("v", []byte(rw))) {
		t.Error("OwnsPolicy")
	}
}
