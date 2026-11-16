package store

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeRoles(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
		err  string
	}{
		{in: []string{"infer"}, want: "infer"},
		{in: []string{"admin", "metrics", "infer"}, want: "infer,metrics,admin"},
		{in: []string{"metrics", " metrics "}, want: "metrics"},
		{in: []string{"root"}, err: `role "root" must be one of infer, metrics, admin`},
		{in: []string{}, err: "at least one role"},
	} {
		got, err := NormalizeRoles(c.in)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: err %v, want %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || strings.Join(got, ",") != c.want {
			t.Errorf("%v: %v %v, want %s", c.in, got, err, c.want)
		}
	}
}

func TestTokenRolesCreateAndPatch(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "r.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, plain, err := db.UpsertToken(ctx, TokenInput{Name: "prometheus", Roles: []string{"metrics"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, roles, _ := db.ClientByToken(ctx, plain); !slices.Equal(roles, []string{"metrics"}) {
		t.Fatalf("created roles = %v", roles)
	}
	if err := db.UpdateToken(ctx, id, nil, []string{"admin", "infer"}, nil, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, roles, _ := db.ClientByToken(ctx, plain); !slices.Equal(roles, []string{"infer", "admin"}) {
		t.Fatalf("patched roles = %v", roles)
	}
	// An explicit empty list is a mistake, not "leave unchanged".
	if err := db.UpdateToken(ctx, id, nil, []string{}, nil, "", nil, nil); err == nil {
		t.Fatal("a key with no role was accepted")
	}
	if _, _, err := db.UpsertToken(ctx, TokenInput{Name: "bad", Roles: []string{"superuser"}}); err == nil {
		t.Fatal("an unknown role was accepted")
	}
}

// v36: admin-only keys could call models before inference checked the infer
// role, so the migration grants infer to keep them working. Other keys are
// untouched.
func TestMigrateV36GrantsInferToAdminTokens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v35.sqlite")
	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	for name, roles := range map[string]string{"ops": "admin", "app": "infer", "both": "infer,admin", "scrape": "metrics"} {
		if err := db.UpsertClientToken(ctx, name, "tok-"+name, roles); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.w.ExecContext(ctx, `UPDATE schema_version SET version = 35`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if got := migrated.SchemaVersion(); got != schemaVersion {
		t.Fatalf("schema version = %d, want %d", got, schemaVersion)
	}
	for name, want := range map[string]string{"ops": "infer,admin", "app": "infer", "both": "infer,admin", "scrape": "metrics"} {
		var roles string
		if err := migrated.r.QueryRowContext(ctx, `SELECT roles FROM client_tokens WHERE name=?`, name).Scan(&roles); err != nil {
			t.Fatal(err)
		}
		if roles != want {
			t.Errorf("%s: roles %q, want %q", name, roles, want)
		}
	}
}
