package s3client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

type statement struct {
	Effect    string                         `json:"Effect"`
	Action    []string                       `json:"Action"`
	Resource  []string                       `json:"Resource"`
	Condition map[string]map[string][]string `json:"Condition,omitempty"`
}

type policyDocument struct {
	Version   string      `json:"Version"`
	Statement []statement `json:"Statement"`
}

// AccessPolicy is the IAM policy of a BucketAccess: read (and, with write,
// write and delete) the objects of the bucket under prefix, and list them. An
// empty prefix is the whole bucket. The document is deterministic, so its
// digest names it (PolicyName).
func AccessPolicy(bucket, prefix string, write bool) []byte {
	objects := []string{"s3:GetObject", "s3:GetObjectVersion"}
	list := []string{"s3:ListBucket"}
	if write {
		objects = append(objects, "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts")
		list = append(list, "s3:ListBucketMultipartUploads")
	}
	listing := statement{Effect: "Allow", Action: list, Resource: []string{"arn:aws:s3:::" + bucket}}
	if prefix != "" {
		listing.Condition = map[string]map[string][]string{"StringLike": {"s3:prefix": {prefix + "*"}}}
	}
	doc, _ := json.Marshal(policyDocument{Version: "2012-10-17", Statement: []statement{
		{Effect: "Allow", Action: objects, Resource: []string{"arn:aws:s3:::" + bucket + "/" + prefix + "*"}},
		{Effect: "Allow", Action: []string{"s3:GetBucketLocation"}, Resource: []string{"arn:aws:s3:::" + bucket}},
		listing,
	}}) // plain strings and slices always marshal
	return doc
}

// PolicyName names a policy document after its user: the digest changes with
// the document, so a spec change attaches a new policy and the one it
// replaces is removed. It is at most 63 bytes.
func PolicyName(user string, doc []byte) string {
	sum := sha256.Sum256(doc)
	return user + "-" + hex.EncodeToString(sum[:4])
}

// OwnsPolicy reports whether a policy name is one PolicyName gave to user.
func OwnsPolicy(user, policy string) bool { return strings.HasPrefix(policy, user+"-") }
