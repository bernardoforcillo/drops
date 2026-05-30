package redis

import "context"

// Credentials is what a CredentialsProvider yields when a new
// connection authenticates. Both fields are optional:
//
//   - Empty Password skips AUTH altogether (Redis without requirepass).
//   - Empty Username with a non-empty Password sends the legacy
//     single-argument AUTH (compatible with Redis < 6).
//   - Both non-empty sends the ACL form: AUTH <user> <pass>.
type Credentials struct {
	Username string
	Password string
}

// CredentialsProvider returns fresh credentials each time a new
// connection authenticates. Use it to integrate short-lived auth
// tokens — AWS ElastiCache IAM, Azure AAD, OIDC, HashiCorp Vault
// leases — that need refresh between dials.
//
// The provider is called once per *new* connection (not once per
// command), and the context passed in is the caller's request
// context so providers can honour deadlines and cancellation.
//
// If the provider returns an error the dial fails with that error
// wrapped; the connection is never added to the pool.
type CredentialsProvider func(ctx context.Context) (Credentials, error)

// StaticCredentials returns a CredentialsProvider that always yields
// the same (username, password) pair. Equivalent to setting
// Options.Username / Options.Password.
func StaticCredentials(username, password string) CredentialsProvider {
	c := Credentials{Username: username, Password: password}
	return func(context.Context) (Credentials, error) { return c, nil }
}
