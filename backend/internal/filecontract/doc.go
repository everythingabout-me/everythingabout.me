// Package filecontract defines pure boundary helpers and wire types for the future
// unified /api/files API. It is not connected to the current router or services.
//
// Paths preserve accepted bytes, including decomposed Unicode and spaces. Parsing
// validates account-absolute paths; Relative removes only the boundary slash. Name
// equivalence for scope checks is supplied by the backend and never rewrites paths.
// Backend-specific name limits, collision detection, symlink isolation and native
// root containment still belong to drivers and services.
//
// Credential selection is not authentication. A present Authorization header must
// select Bearer even when malformed; callers must verify the selected credential.
// Cookie mutations require an explicit approved Origin and X-CSRF-Token checked by
// a session-bound validator. The configured canonical origin is trusted input;
// Host, Forwarded and X-Forwarded-* headers cannot establish origin trust. Missing
// Origin is denied on cookie mutations, including raw and part PUTs. Tokens are
// opaque: the helper neither decodes them nor treats presence as validity.
//
// Revision helpers parse and evaluate public preconditions only. They cannot
// supply native atomicity, fence delayed writes, authenticate a principal, enforce
// quota, persist an Operation, or establish that its effects are terminal. In
// particular outcome_unknown must retain conflicting mutation guards in the future
// durable service; the DTO is not an implementation of that recovery protocol.
// RootBinding captures immutable root identity independently of credential rotation.
//
// Tests exercise these helpers and test-only HTTP adapters, not nonexistent runtime
// endpoints. Native safety probes, database workflows, CORS, streaming/ranges and
// production route cutover are deliberately outside this package's P0 scope.
package filecontract
