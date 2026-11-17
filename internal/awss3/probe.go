package awss3

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// Probe checks that the account is usable: STS GetCallerIdentity (the
// credentials are valid, and a role, if one is configured, was assumed) and S3
// ListBuckets (the identity may use S3; it needs s3:ListAllMyBuckets). It
// reads nothing from the answers: the account and the identity stay out of the
// status. The idle connections of cfg's HTTP client are closed on return, so a
// configuration built for one probe leaves nothing behind.
func Probe(ctx context.Context, cfg aws.Config) error {
	defer closeIdle(cfg.HTTPClient)
	api := sts.NewFromConfig(cfg, func(o *sts.Options) { o.RetryMaxAttempts = 1 })
	if _, err := api.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err != nil {
		return fail(ctx, "STS GetCallerIdentity", err)
	}
	_, err := New(cfg).ServerVersion(ctx)
	if errors.Is(err, s3client.ErrNoVersion) {
		return nil
	}
	return err
}
