package s3client

import (
	"crypto/sha256"
	"encoding/base32"
	"strconv"

	"github.com/craftainer-labs/lib-operator-go/pkg/naming"
)

// Name limits (ADR 0002 section 10).
const (
	// maxBucketLen is the S3 bucket name limit.
	maxBucketLen = 63
	// maxUserLen leaves room for "-" and the 8-character policy digest in a
	// policy name of at most 63 bytes.
	maxUserLen = 54
)

// BucketName is the bucket of a Bucket claim. Buckets can hold the ownership
// marker (a tag), so the name carries the short hash.
func BucketName(ns, name string) string {
	return naming.Tenant(ns, name, maxBucketLen, "-", naming.HashShort)
}

// UserName is the IAM user behind a BucketAccess claim. RustFS users cannot
// hold a marker, so the name is the only barrier and carries the long hash.
func UserName(ns, name string) string {
	return naming.Tenant(ns, name, maxUserLen, "-", naming.HashLong)
}

var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// AccessKey is the access key id of generation seq of a claim: 20 characters
// derived from the claim's UID, so a retry after a crash issues the same key
// instead of leaking another, and no other claim can derive it.
func AccessKey(uid string, seq int64) string {
	sum := sha256.Sum256([]byte(uid + ":" + strconv.FormatInt(seq, 10)))
	return keyEncoding.EncodeToString(sum[:])[:20]
}
