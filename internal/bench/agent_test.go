package bench

import (
	"bytes"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

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
