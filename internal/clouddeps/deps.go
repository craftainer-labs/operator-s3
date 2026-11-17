//go:build tools

// Package clouddeps pins the cloud SDK modules in go.mod before the cloud
// backends (NKs3-1 to NKs3-3) import them: `go mod tidy` drops a requirement
// nothing imports, and the dependency-only change NKs3-0 must keep them and
// their go.sum entries. The tools build tag keeps this file out of every
// build, test and the coverage gate; tidy still reads it. Each backend item
// deletes its cloud's lines once its code imports the same packages, and the
// last one deletes this package. NKs3-1a-ii removed aws-sdk-go-v2/service/s3 and
// NKs3-1b service/iam, which internal/awss3 imports now.
//
// Deferred (not pinned): armauthorization and armstorage, for the Azure Entra
// RBAC path, which NKs3-3 adds if it needs them.
package clouddeps

import (
	_ "cloud.google.com/go/storage"
	_ "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	_ "google.golang.org/api/iam/v1"
)
