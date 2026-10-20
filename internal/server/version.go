package server

// Version is the single source of truth for the ezllm version string.
//
// It is reported by GET /healthz (public) and by the /admin/overview health
// object (rendered in the dashboard health strip), and it must match the
// "Current:" line in VERSION.md — version_test.go asserts that pairing, so a
// bump that updates only one side fails the suite.
//
// Versioning scheme (see VERSION.md): one version per change date, all
// patches on a date share a version, patch component increments per date.
const Version = "0.0.21"
