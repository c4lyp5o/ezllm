package geminiweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The init page carries the five tokens as JSON blobs; build a fixture that
// looks like the real page (get_access_token.py scrapes these patterns).
const initPageFixture = `<html><script>
var data = {"SNlM0e":"tok-abc123","cfb2h":"boq_lbl_2025","FdrFJe":"sid_42","TuX5cc":"en","qKIAYe":"feeds/push42"};
</script></html>`

func newMockServer(t *testing.T, initPage string, rpcHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(endpointInit, func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(initPage)); err != nil {
			t.Errorf("write init page: %v", err)
		}
	})
	mux.HandleFunc(endpointBatchExec, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("rpcids") != rpcGetUserStatus {
			t.Errorf("batchexecute missing rpcids: rpcids=%q", r.Form.Get("rpcids"))
		}
		rpcHandler(w, r)
	})
	return httptest.NewServer(mux)
}

// wrbFrame wraps a payload in the length-prefixed frame format.
func wrbFrame(payload string) string {
	return fmt.Sprintf("%d\n%s", utf16Units(payload)+1, payload)
}

// statusPart builds one batchexecute frame part for GET_USER_STATUS:
// ["wrb.fr","otAQ7b","<part_body json>",null,null,null,"generic"]
// where part_body is the JSPB array with status@14, models@15, tier@16,
// capabilities@17.
func statusPart(t *testing.T, partBody []any) string {
	t.Helper()
	body, err := json.Marshal(partBody)
	if err != nil {
		t.Fatal(err)
	}
	// In a real batchexecute response part[2] is a STRING containing JSON
	// (the Python client json.loads it) — double-encode to match the wire.
	bodyStr, err := json.Marshal(string(body))
	if err != nil {
		t.Fatal(err)
	}
	part, err := json.Marshal([]any{"wrb.fr", rpcGetUserStatus, json.RawMessage(bodyStr), nil, nil, nil, "generic"})
	if err != nil {
		t.Fatal(err)
	}
	return wrbFrame(string(part))
}

func TestParseKey(t *testing.T) {
	c, err := ParseKey("__Secure-1PSID=abc\n__Secure-1PSIDTS=def")
	if err != nil {
		t.Fatal(err)
	}
	if c.psid != "abc" || c.psidts != "def" {
		t.Fatalf("got %+v", c)
	}
	// Round trip: a rotated cookie serializes back to the stored format.
	c.psidts = "rotated"
	if k := c.Key(); !strings.Contains(k, "__Secure-1PSIDTS=rotated") || !strings.Contains(k, "__Secure-1PSID=abc") {
		t.Fatalf("Key() = %q", k)
	}
	// PSID alone is legal (rotation refetches PSIDTS).
	if _, err := ParseKey("__Secure-1PSID=abc"); err != nil {
		t.Fatalf("PSID only should parse: %v", err)
	}
	// No PSID is not.
	if _, err := ParseKey("__Secure-1PSIDTS=def\n"); err == nil {
		t.Fatal("missing PSID must fail")
	}
}

// TestParseKeyWholeCookiePaste covers what an operator actually copies out of a
// browser: ONE semicolon-separated header carrying the whole jar. The old
// line-wise split stored everything after __Secure-1PSID as its value, so the
// session that reached Google was garbage.
func TestParseKeyWholeCookiePaste(t *testing.T) {
	whole := "cookie: __Secure-1PSID=g.a000REALPSID; __Secure-1PSIDTS=sidts.abc123==; " +
		"SID=foo; HSID=bar; SSID=baz; APISID=x; SAPISID=y; __Secure-1PSIDCC=cc; SIDCC=zz"

	c, err := ParseKey(whole)
	if err != nil {
		t.Fatalf("whole-cookie paste rejected: %v", err)
	}
	if c.psid != "g.a000REALPSID" {
		t.Errorf("psid = %q, want the cookie value alone (no trailing jar)", c.psid)
	}
	if strings.Contains(c.psid, ";") {
		t.Errorf("psid still carries the rest of the header: %q", c.psid)
	}
	if c.psidts != "sidts.abc123==" {
		t.Errorf("psidts = %q, want exact value (padding preserved)", c.psidts)
	}
}

// document.cookie has no "Cookie:" prefix — same jar, no header name.
func TestParseKeyDocumentCookiePaste(t *testing.T) {
	c, err := ParseKey("__Secure-1PSID=abc; SID=x; __Secure-1PSIDTS=def")
	if err != nil {
		t.Fatal(err)
	}
	if c.psid != "abc" || c.psidts != "def" {
		t.Fatalf("got psid=%q psidts=%q", c.psid, c.psidts)
	}
}

// The prefix trap: __Secure-1PSIDCC is a DIFFERENT cookie. An exact-match is the
// safety property — a prefix match would silently store the wrong credential.
func TestParseKeyDoesNotMatchPrefixSibling(t *testing.T) {
	if _, err := ParseKey("__Secure-1PSIDCC=ccvalue; SIDCC=zz"); err == nil {
		t.Fatal("__Secure-1PSIDCC must not satisfy the required __Secure-1PSID")
	}
	// And it must not contaminate the real one when both are present.
	c, err := ParseKey("__Secure-1PSIDCC=cc; __Secure-1PSID=real; SIDCC=z")
	if err != nil {
		t.Fatal(err)
	}
	if c.psid != "real" {
		t.Fatalf("psid = %q, want %q", c.psid, "real")
	}
}

// DevTools sometimes wraps values in quotes when copied.
func TestParseKeyStripsQuoting(t *testing.T) {
	c, err := ParseKey("__Secure-1PSID=\"abc\"\n__Secure-1PSIDTS=\"def\"")
	if err != nil {
		t.Fatal(err)
	}
	if c.psid != "abc" || c.psidts != "def" {
		t.Fatalf("got %+v", c)
	}
}

func TestInitScrapesTokens(t *testing.T) {
	var gotRpcids string
	srv := newMockServer(t, initPageFixture, func(w http.ResponseWriter, r *http.Request) {
		gotRpcids = r.Form.Get("rpcids")
		// Serve a real status body so GetUserStatus exercises the full path.
		partBody := make([]any, 18)
		partBody[14] = 1000
		partBody[15] = []any{modelDescriptor("abc", "Pro", "Gemini 3 Pro", "d", 3)}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, ")]}'\n"+statusPart(t, partBody))
	})
	defer srv.Close()

	c := New(cookie{psid: "abc", psidts: "def"})
	c.base = srv.URL
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.tokens.accessToken != "tok-abc123" {
		t.Errorf("accessToken = %q", c.tokens.accessToken)
	}
	if c.tokens.buildLabel != "boq_lbl_2025" || c.tokens.sessionID != "sid_42" ||
		c.tokens.language != "en" || c.tokens.pushID != "feeds/push42" {
		t.Errorf("tokens = %+v", c.tokens)
	}

	if _, err := c.GetUserStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotRpcids != rpcGetUserStatus {
		t.Errorf("request rpcids=%q, want %s", gotRpcids, rpcGetUserStatus)
	}
}

func TestInitExpiredCookies(t *testing.T) {
	srv := newMockServer(t, `<html>sign in to continue</html>`, func(http.ResponseWriter, *http.Request) {})
	defer srv.Close()
	c := New(cookie{psid: "stale"})
	c.base = srv.URL
	if err := c.Init(context.Background()); !errors.Is(err, ErrNoAccessToken) {
		t.Fatalf("err = %v, want ErrNoAccessToken", err)
	}
}

// modelDescriptor builds one AvailableModel descriptor per from_rpc:
// id@0, category@10, display@11, description@12, model_number@17.
func modelDescriptor(id, category, display, desc string, number int) []any {
	d := make([]any, 18)
	d[0] = id
	d[10] = category
	d[11] = display
	d[12] = desc
	d[17] = number
	return d
}

func TestGetUserStatusAvailable(t *testing.T) {
	partBody := make([]any, 18)
	partBody[14] = 1000
	partBody[15] = []any{
		modelDescriptor("9d8ca3786ebdfbea", "Pro", "Gemini 3 Pro", "advanced model", 3),
		modelDescriptor("fbb127bbb056c959", "Flash", "Gemini 3 Flash", "fast model", 1),
	}
	partBody[16] = []any{8}   // tier flag 8 -> Pro capacity 2/12
	partBody[17] = []any{115} // capability 115 wins over tier flags -> Plus 4/12
	srv := newMockServer(t, initPageFixture, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, ")]}'\n"+statusPart(t, partBody))
	})
	defer srv.Close()

	c := New(cookie{psid: "abc"})
	c.base = srv.URL
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	us, err := c.GetUserStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if us.Status != statusAvailable {
		t.Fatalf("status = %+v", us.Status)
	}
	if len(us.Models) != 2 {
		t.Fatalf("models = %+v", us.Models)
	}
	m := us.Models[1] // "Flash" is sorted first? No — keep discovery order: models come in RPC order
	if m.ID != "fbb127bbb056c959" {
		t.Fatalf("model[1] = %+v", us.Models)
	}
	// capability 115 must win over tier flag 8: Plus = 4/12, not Pro = 2/12.
	if m.Capacity != 4 || m.CapacityField != 12 {
		t.Errorf("capacity = %d/%d, want 4/12 (capability 115 wins)", m.Capacity, m.CapacityField)
	}
	if m.Name != "gemini-flash" {
		t.Errorf("derived name = %q", m.Name)
	}
	foundAlias := false
	for _, a := range m.Aliases {
		if a == "gemini-flash" || a == "flash" {
			foundAlias = true
		}
	}
	if !foundAlias {
		t.Errorf("aliases = %v", m.Aliases)
	}
	if m.ModelNumber != 1 {
		t.Errorf("model_number = %d, want 1", m.ModelNumber)
	}
	// The dial header must carry the model id, capacity tail and number.
	hdr := m.modelHeader()
	want := `[1,null,null,null,"fbb127bbb056c959",null,null,0,[4,5,6,8],null,null,4,null,null,1]`
	if hdr[modelHeaderKey] != want {
		t.Errorf("model header\n got %s\nwant %s", hdr[modelHeaderKey], want)
	}
	if hdr[headerAux1] != "[0]" || hdr[headerAux2] != "[0,0,0]" {
		t.Errorf("aux headers = %v", hdr)
	}
}

func TestGetUserStatusUnauthenticated(t *testing.T) {
	partBody := make([]any, 18)
	partBody[14] = 1016 // UNAUTHENTICATED
	srv := newMockServer(t, initPageFixture, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, ")]}'\n"+statusPart(t, partBody))
	})
	defer srv.Close()
	c := New(cookie{psid: "stale"})
	c.base = srv.URL
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	us, err := c.GetUserStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if us.Status != statusUnauthenticated {
		t.Fatalf("status = %+v, want UNAUTHENTICATED", us.Status)
	}
	if us.Models != nil {
		t.Errorf("unauthenticated must yield no models, got %v", us.Models)
	}
}

func TestRotateCookies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__Secure-1PSIDTS", Value: "fresh-ts"})
		fmt.Fprint(w, "[]")
	}))
	defer srv.Close()
	c := New(cookie{psid: "abc", psidts: "old-ts"})
	c.rotateURL = srv.URL + "/RotateCookies"
	key, err := c.Rotate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(key, "__Secure-1PSIDTS=fresh-ts") {
		t.Fatalf("rotated key = %q", key)
	}
	if c.cookies.psidts != "fresh-ts" {
		t.Errorf("client cookie not updated: %q", c.cookies.psidts)
	}
}

func TestComputeCapacityTiers(t *testing.T) {
	for _, tc := range []struct {
		tier, cap []int
		wantC     int
		wantF     int
	}{
		{nil, []int{115}, 4, 12}, // Plus
		{[]int{16}, nil, 3, 12},  // Pro uncommon
		{[]int{8}, nil, 2, 12},   // Pro
		{nil, []int{19}, 2, 12},  // Pro via capability
		{nil, nil, 1, 12},        // Free
		{[]int{21}, nil, 1, 13},  // field-13 override
	} {
		c, f := computeCapacity(tc.tier, tc.cap)
		if c != tc.wantC || f != tc.wantF {
			t.Errorf("computeCapacity(%v,%v) = %d/%d, want %d/%d", tc.tier, tc.cap, c, f, tc.wantC, tc.wantF)
		}
	}
}

func TestDeriveNameAndAliases(t *testing.T) {
	name, aliases := deriveNameAndAliases("abc123", "Pro", "Gemini 3 Pro")
	if name != "gemini-pro" {
		t.Errorf("name = %q", name)
	}
	for _, want := range []string{"abc123", "pro", "gemini-pro", "gemini-3-pro"} {
		found := false
		for _, a := range aliases {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("aliases missing %q: %v", want, aliases)
		}
	}
	// Display-only category — faithful to the Python client's quirk: a display
	// name that already starts with "Gemini" yields "gemini-gemini-flash"
	// (while the clean "gemini-flash" is in the alias set).
	name2, aliases2 := deriveNameAndAliases("x", "", "Gemini Flash")
	if name2 != "gemini-gemini-flash" {
		t.Errorf("display-only name = %q", name2)
	}
	foundClean := false
	for _, a := range aliases2 {
		if a == "gemini-flash" {
			foundClean = true
		}
	}
	if !foundClean {
		t.Errorf("clean alias missing: %v", aliases)
	}
}
