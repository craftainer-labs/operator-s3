package awss3

import (
	"context"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// testConfig is an AWS configuration with fixed credentials that reaches the
// server at url, as cloud.Egress.AWS builds one for an endpoint override.
func testConfig(url, region string, hc *http.Client) aws.Config {
	return aws.Config{
		Region: region, BaseEndpoint: aws.String(url), HTTPClient: hc,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test-access", SecretAccessKey: "test-secret"}, nil
		}),
	}
}
