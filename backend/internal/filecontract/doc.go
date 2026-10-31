// Package filecontract defines pure boundary helpers and wire types for the
// unified /api/files API. P2 runtime handlers reuse the path/auth/resource helpers;
// this package alone supplies no storage or database guarantees.
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
// Tests here exercise pure helpers and test-only HTTP adapters. Real P2 endpoint,
// CORS, streaming/range and database integration tests live in the api package;
// native safety and durable-write recovery remain separate conformance gates.
package filecontract
