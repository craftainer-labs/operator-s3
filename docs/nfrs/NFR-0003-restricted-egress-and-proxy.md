# NFR-0003. A tenant cannot aim the operator at internal addresses

## Status

Implemented

## Attribute

Security (server-side request forgery, multi-tenancy, plan rows EB-3 and EB-5).

## Description

A namespaced ObjectStore lets a tenant name any host. Decided by the user on
2026-11-03:

- Every connection is checked after the host name has been resolved, when the
  socket is about to connect (`net.Dialer.Control`), so a name that resolves
  into a blocked range is stopped as well as a literal address. The store's
  proxy host is checked the same way. net/http hands the dialer a host name,
  and behind a proxy the proxy would resolve it where this policy cannot see
  the answer, so `HostDialer` resolves the name itself, refuses it when any of
  its addresses is blocked, and asks the proxy for an address, never the name.
  A non-IP destination that reaches the proxied dialer is refused (fail
  closed).
- An IPv6 address that carries an IPv4 address is checked as that IPv4 address:
  IPv4-mapped (`::ffff:`), NAT64 (`64:ff9b::/96`, `64:ff9b:1::/48`), 6to4
  (`2002::/16`) and Teredo (`2001::/32`). The always-blocked set also holds
  192.0.0.0/24, 198.18.0.0/15 and 168.63.129.16.
- The environment variable `EGRESS_MODE` (flag `--egress-mode`) selects what is
  blocked. `strict`, the default, blocks loopback, link-local (which holds the
  cloud metadata address 169.254.169.254), the other metadata addresses,
  multicast and reserved ranges, and the private ranges (RFC 1918, CGNAT
  100.64.0.0/10, ULA fc00::/7). `allow-private` blocks all but the private
  ranges, for clusters whose object stores are on private addresses. Any other
  value fails startup.
- Neither `endpoint` nor `proxyURL` may carry `user:password@`: the webhooks
  reject it, and the client and the dialer refuse such a URL even when
  admission was bypassed. Credentials come only from Secret references. URLs
  and their userinfo are never echoed in errors, conditions or Events.
- The proxy may be `http`, `https` (CONNECT, TLS 1.2 or newer) or `socks5`. A
  proxy URL without a port uses the scheme's default: 80, 443 and 1080.
- A failed request reaches conditions and Events as one fixed text per category
  (refused by the egress policy, TLS handshake failed, authentication failed,
  timed out, connection failed), and an HTTP error as its status alone, never
  an address, a store-supplied code or message, or the raw error, so a tenant
  gets no port scan or name resolution out of the operator and a hostile store
  cannot write into a condition. The raw error is in the operator log.

## Source

Plan Decision log, 2026-11-03 (ADR 0002 open items) and 2026-11-13 (NO2b
review); plan rows EB-3 and EB-5;
[ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md), "Open issues".

## Verification

`TestPolicyCheck`, `TestControl`, `TestDirectDialIsCheckedAfterResolution`,
`TestHTTPProxyTunnel`, `TestHTTPSProxyTunnel`, `TestSOCKS5ChecksTheDestination`,
`TestCheckTarget`, `TestProxyAddrDefaultPorts`, `TestProxyUserinfoAndScheme`,
`TestHostDialerWithoutAProxyIsTheDialer` and
`TestHostDialerResolvesBeforeTheProxy` (`internal/egress`) cover the range
tables of both modes and the dialers; `TestNewRejectsWhatItCannotUse`,
`TestAnswersBecomeFixedErrors` and `TestTransportFailuresAreCategorised`
(`internal/s3client`) cover the fixed texts and the endpoint rules;
`TestObjectStoreValidator`, `TestClusterObjectStoreValidator` and
`TestStoreErrorsDoNotEchoTheURL` (`internal/webhook`) and
`TestBucketOnAnInvalidStore` (`internal/controller`) cover the userinfo
rejection; `TestControllerSettings` and `TestRunRejectsInvalidSettings`
(`internal/app`) cover the environment variable.
