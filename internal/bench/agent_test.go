package bench

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/backend"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/memory"
)

type storerLoader struct{ st storage.Storer }

func (l storerLoader) Load(*url.URL) (storage.Storer, error) { return l.st, nil }

// receivePackBodies serves one in-memory repo over smart HTTP and keeps the
// body of every receive-pack request, where push options travel.
func receivePackBodies(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies []string
	)
	h := backend.New(storerLoader{memory.NewStorage()})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			bodies = append(bodies, string(b))
			mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return bodies
	}
}

func TestAgentPushSendsPushOptions(t *testing.T) {
	t.Parallel()

	srv, bodies := receivePackBodies(t)
	opts := []string{"ci.skip", "merge_request.label=a,b"}
	a, err := newAgent(0, "repo.git", srv.URL, "refs/forgemark/fmX-c1-a0", formatcfg.SHA1,
		&CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 16},
		staticCreds{username: "u", password: "p"}, srv.Client(), opts, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.commit(rand.New(rand.NewSource(1)), 0); err != nil {
		t.Fatal(err)
	}
	if err := a.push(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.deleteRef(a.ref)

	got := bodies()
	if len(got) != 2 {
		t.Fatalf("server saw %d receive-pack requests, want a push and a delete", len(got))
	}
	for i, body := range got {
		for _, o := range opts {
			if !strings.Contains(body, o) {
				t.Errorf("receive-pack request %d is missing push option %q", i, o)
			}
		}
	}
}

// receivePackAdvertisement is a smart-HTTP receive-pack info/refs reply for an
// empty repo advertising caps.
func receivePackAdvertisement(caps string) string {
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	return pkt("# service=git-receive-pack\n") + "0000" +
		pkt("0000000000000000000000000000000000000000 capabilities^{}\x00"+caps+"\n") + "0000"
}

func TestAdvertisesPushOptions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		caps string
		want bool
	}{
		{"report-status delete-refs push-options", true},
		{"report-status delete-refs", false},
		{"report-status push-optionsx", false},
	} {
		got, err := advertisesPushOptions([]byte(receivePackAdvertisement(tc.caps)))
		if err != nil {
			t.Fatalf("caps %q: %v", tc.caps, err)
		}
		if got != tc.want {
			t.Errorf("caps %q: advertisesPushOptions = %v, want %v", tc.caps, got, tc.want)
		}
	}
	if _, err := advertisesPushOptions([]byte("<html>not git</html>")); err == nil {
		t.Error("a non-git reply must be an error, not a missing capability")
	}
}

// go-git drops push options the server doesn't advertise and the push still
// succeeds, so the run must refuse to start rather than measure pushes that
// differ from the ones requested.
func TestNewRunnerRequiresPushOptionsCapability(t *testing.T) {
	t.Parallel()

	noCaps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		_, _ = io.WriteString(w, receivePackAdvertisement("report-status delete-refs"))
	}))
	defer noCaps.Close()
	withCaps, _ := receivePackBodies(t)

	w := Workload{
		Strategy: "branch", Concurrency: []int{1}, Duration: time.Second,
		Commit: CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1},
	}
	target := func(remote string) Target {
		return Target{Remote: remote, Repos: []string{"repo.git"}, Secret: "s"}
	}

	if r, err := NewRunner(t.Context(), target(noCaps.URL), w, nil); err != nil {
		t.Fatalf("without push options the capability must not be probed: %v", err)
	} else {
		r.Close()
	}

	w.PushOptions = []string{"ci.skip"}
	_, err := NewRunner(t.Context(), target(noCaps.URL), w, nil)
	if err == nil || !strings.Contains(err.Error(), "push-options") {
		t.Fatalf("NewRunner against a server without push-options = %v, want a push-options error", err)
	}

	r, err := NewRunner(t.Context(), target(withCaps.URL), w, nil)
	if err != nil {
		t.Fatalf("NewRunner against a server advertising push-options: %v", err)
	}
	r.Close()
}
