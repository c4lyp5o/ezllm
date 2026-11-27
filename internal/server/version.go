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
const Version = "2.3.0"

// DocsRef is the git ref whose docs match this build: the dashboard links there.
// publish.yml and release-assets.sh set it (-ldflags -X) to the release tag, or to
// development for a development image; any other build links to master.
var DocsRef = "master"

// docsBase is where the repo-relative paths in GET /admin/bridges open.
func docsBase() string { return "https://github.com/c4lyp5o/ezllm/blob/" + DocsRef + "/" }
