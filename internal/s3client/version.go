package s3client

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ErrNoVersion means the store answered its server info without a version.
var ErrNoVersion = errors.New("the store reported no server version")

// ServerVersion asks the admin API for the server info (GET
// /rustfs/admin/v3/info, the ServerInfoAdminAction) and returns the version of
// the first server that reports one. The call is authenticated, so a store
// that answers proves both that it is reachable and that the admin
// credentials are accepted; a failure is one of the categories of this
// package (ErrConnTLS, ErrConnAuth, ...) or an *Error, never anything that
// names the host or a key.
//
// RustFS reports "<commit date>@<tag, short commit or package version>" in
// each server's "version" field; only the part after the "@" is returned. A
// store that answers 200 and names no version gives ErrNoVersion.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	data, err := c.admin(ctx, http.MethodGet, "/info", nil, nil)
	if err != nil {
		return "", err
	}
	var info struct {
		Info struct {
			Servers []struct {
				Version string `json:"version"`
			} `json:"servers"`
		} `json:"info"`
	}
	if err := decodeJSON(data, &info); err != nil {
		return "", err
	}
	for _, s := range info.Info.Servers {
		if _, v, ok := strings.Cut(s.Version, "@"); ok {
			s.Version = v
		}
		if v := strings.TrimSpace(s.Version); v != "" {
			return v, nil
		}
	}
	return "", ErrNoVersion
}
