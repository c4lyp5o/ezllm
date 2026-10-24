package server

// Version is the single source of truth for the ezllm version string.
//
// It is reported by GET /healthz (public) and by the /admin/overview health
// object (rendered in the dashboard health strip), and it must match the
// "Current:" line in VERSION.md — version_test.go asserts that pairing, so a
// bump that updates only one side fails the suite.
//
// Versioning scheme (see VERSION.md): semantic versioning from 1.0.0; a
// release tag vX.Y.Z must match this constant (the release workflows check).
const Version = "1.2.0"
