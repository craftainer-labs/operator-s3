package s3client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
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

// Partition is the AWS partition of a region: aws-cn for the China regions,
// aws-us-gov for GovCloud, aws-iso, aws-iso-b, aws-iso-e and aws-iso-f for the
// isolated regions, and aws for every other. It is the second field of an
// ARN. A self-hosted store has no partition and uses aws, whose ARNs its
// policies are written in.
func Partition(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	case strings.HasPrefix(region, "us-iso-"):
		return "aws-iso"
	case strings.HasPrefix(region, "us-isob-"):
		return "aws-iso-b"
	case strings.HasPrefix(region, "eu-isoe-"):
		return "aws-iso-e"
	case strings.HasPrefix(region, "us-isof-"):
		return "aws-iso-f"
	}
	return "aws"
}

// AccessPolicy is the IAM policy of a BucketAccess: read (and, with write,
// write and delete) the objects of the bucket under prefix, and list them. An
// empty prefix is the whole bucket. The bucket's ARN is in the partition. The
// document is deterministic, so its digest names it (PolicyName).
func AccessPolicy(partition, bucket, prefix string, write bool) []byte {
	arn := "arn:" + partition + ":s3:::" + bucket
	objects := []string{"s3:GetObject", "s3:GetObjectVersion"}
	list := []string{"s3:ListBucket"}
	if write {
		objects = append(objects, "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts")
		list = append(list, "s3:ListBucketMultipartUploads")
	}
	listing := statement{Effect: "Allow", Action: list, Resource: []string{arn}}
	if prefix != "" {
		listing.Condition = map[string]map[string][]string{"StringLike": {"s3:prefix": {prefix + "*"}}}
	}
	doc, _ := json.Marshal(policyDocument{Version: "2012-10-17", Statement: []statement{
		{Effect: "Allow", Action: objects, Resource: []string{arn + "/" + prefix + "*"}},
		{Effect: "Allow", Action: []string{"s3:GetBucketLocation"}, Resource: []string{arn}},
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

// ownedPolicy is the exact shape PolicyName gives: the user, a dash and eight
// hex characters. A looser prefix match would also take a policy an admin
// attached by hand, or one of another user whose name starts with this one.
var ownedPolicy = regexp.MustCompile(`^(.*)-[0-9a-f]{8}$`)

// OwnsPolicy reports whether a policy name is one PolicyName gave to user.
func OwnsPolicy(user, policy string) bool {
	m := ownedPolicy.FindStringSubmatch(policy)
	return m != nil && m[1] == user
}
