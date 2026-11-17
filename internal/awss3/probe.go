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
// returns the account id of the answer, which the store reports as
// status.discovered.accountId; the identity's own ARN stays out of the status.
// The idle connections of cfg's HTTP client are closed on return, so a
// configuration built for one probe leaves nothing behind.
func Probe(ctx context.Context, cfg aws.Config) (account string, err error) {
	defer closeIdle(cfg.HTTPClient)
	api := sts.NewFromConfig(cfg, func(o *sts.Options) { o.RetryMaxAttempts = 1 })
	id, err := api.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fail(ctx, "STS GetCallerIdentity", err)
	}
	if _, err = New(cfg).ServerVersion(ctx); !errors.Is(err, s3client.ErrNoVersion) {
		return "", err
	}
	return aws.ToString(id.Account), nil
}
