// Package s3storage provides native trees through the official AWS SDK v2, using
// explicit endpoint, region, bucket, addressing style and static credentials with an
// optional session token. It does not read environment credentials or configuration,
// and it publishes no transfer URLs.
//
// Reads are a bound driver. Writes are a separate mutation backend (MutationBackend)
// that the process uses only after this exact connection has been admitted: the
// coordinated object protocol needs conditional create, atomic publication and part
// persistence, and an S3-compatible server may implement all three, some, or none of
// them. Evidence is measured per endpoint, so a second bucket does not inherit the
// first one's answer.
//
// Bound drivers are cheap views over a bounded, process-shared HTTP transport.
// Closing a view cancels its active requests and rejects new calls, while other
// owners keep using the shared pool. Signed requests never
// follow HTTP redirects. The endpoint must explicitly include its scheme and may
// not include a path prefix, user information, query or fragment.
//
// Immediate-child listings consume and validate all native delimiter pages before
// returning a public page. This detects NFC-equivalent names and file/folder
// collisions across native page boundaries. P1 caps validation at 100,000 child
// entries and 1,000 native pages; exceeding either budget returns resource_limit
// without a partial page. Enumeration is not a snapshot against external writes.
// Cursors are authenticated, bound to owner/root/configuration generation and
// directory, and remain valid only for the lifetime of their factory.
//
// PinnedRead is false because versionless S3-compatible endpoints cannot promise
// immutable selection. Individual non-null native version IDs returned by HEAD
// and GET are exposed privately as opaque evidence and passed back as versionId
// on a requested version read. The GET response must confirm that exact version.
// ETags, list timestamps and null version IDs are never strong version evidence.
// Reads validate the actual GET's status, range, size and metadata, stream bounded
// bytes with cancellation, and report short/extra bodies to their readers.
package s3storage
