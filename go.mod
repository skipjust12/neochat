module neochat

// Pinned to a patch release, not a bare "1.25", on purpose: CI resolves
// its toolchain from this line (go-version-file in .github/workflows/ci.yml),
// so whatever is written here is what the vulnerability scan and the
// build actually run on. At 1.25.0 govulncheck reported 23 reachable
// standard-library vulnerabilities -- TLS, x509, net/url, net/http --
// every one of them fixed in a 1.25.x patch. Keep this at a current
// patch release and bump it when govulncheck says to; dropping it back
// to "1.25.0" silently reintroduces all of them. Moved to 1.26 when the
// net/http, crypto/tls and crypto/x509 fixes of November 2026 shipped in
// 1.26.9/1.26.10 only.
go 1.26.10

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/redis/go-redis/v9 v9.22.0
	golang.org/x/image v0.45.0
	golang.org/x/net v0.62.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)
