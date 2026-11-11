// The volume allowed-hosts calls against a control plane that answers as sbcli
// does. The case that matters most is the unrestricted volume: the control plane
// reads an empty allowed-hosts list as `allow every host`, so adding one host to
// such a volume would turn it into a volume that only that host can connect.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

const mdsHost = "nqn.2014-08.io.simplyblock:uuid:11111111-2222-4333-8444-555555555555"

// hostsServer answers the volume's detail with the given allowed hosts, records
// every POST and DELETE to the host routes, and answers them with post and del.
type hostsServer struct {
	allowed []string
	post    func(w http.ResponseWriter)
	del     func(w http.ResponseWriter)
	posts   []string
	deletes []string
}

func (s *hostsServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/volumes/"+testVolume+"/"):
			w.Header().Set("Content-Type", "application/json")
			hosts, _ := json.Marshal(s.allowed)
			_, _ = w.Write([]byte(`{"id":"` + testVolume + `","name":"vol1","pool_name":"pool1","size":1,` +
				`"ns_id":1,"nqn":"nqn.x","allowed_hosts":` + string(hosts) + `}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/volumes/"+testVolume+"/hosts"):
			var body struct {
				HostNQN string `json:"host_nqn"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.posts = append(s.posts, body.HostNQN)
			s.post(w)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/volumes/"+testVolume+"/hosts/"):
			s.deletes = append(s.deletes, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			s.del(w)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func created(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"nqn":"` + mdsHost + `"}`))
}

func answer(code int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

func TestEnsureVolumeHostLeavesAnUnrestrictedVolumeAlone(t *testing.T) {
	s := &hostsServer{allowed: []string{}, post: created}
	c := newTestClient(t, s.handler(t))
	got, err := c.EnsureVolumeHost(context.Background(), testHandle, mdsHost)
	if err != nil {
		t.Fatal(err)
	}
	if got != HostNotNeeded {
		t.Errorf("grant = %d, want HostNotNeeded", got)
	}
	if len(s.posts) != 0 {
		t.Errorf("added %v to a volume that allows every host, which would lock the others out", s.posts)
	}
}

func TestEnsureVolumeHostAddsAHostToARestrictedVolume(t *testing.T) {
	s := &hostsServer{allowed: []string{"nqn.2014-08.io.simplyblock:uuid:csi-node"}, post: created}
	c := newTestClient(t, s.handler(t))
	got, err := c.EnsureVolumeHost(context.Background(), testHandle, mdsHost)
	if err != nil {
		t.Fatal(err)
	}
	if got != HostAdded || len(s.posts) != 1 || s.posts[0] != mdsHost {
		t.Errorf("grant = %d, posts = %v", got, s.posts)
	}
}

func TestEnsureVolumeHostDoesNotAddAHostTwice(t *testing.T) {
	s := &hostsServer{allowed: []string{"nqn.other", mdsHost}, post: created}
	c := newTestClient(t, s.handler(t))
	got, err := c.EnsureVolumeHost(context.Background(), testHandle, mdsHost)
	if err != nil {
		t.Fatal(err)
	}
	if got != HostAlreadyAllowed || len(s.posts) != 0 {
		t.Errorf("grant = %d, posts = %v", got, s.posts)
	}
}

// Two reconciles can race between the read and the add. The control plane then
// answers 400 `already allowed`, which is the outcome that was wanted.
func TestEnsureVolumeHostTakesALostRaceAsDone(t *testing.T) {
	s := &hostsServer{
		allowed: []string{"nqn.other"},
		post:    answer(http.StatusBadRequest, `{"detail":"Host `+mdsHost+` is already allowed"}`),
	}
	c := newTestClient(t, s.handler(t))
	got, err := c.EnsureVolumeHost(context.Background(), testHandle, mdsHost)
	if err != nil || got != HostAlreadyAllowed {
		t.Errorf("grant = %d, err = %v", got, err)
	}
}

func TestEnsureVolumeHostReportsARefusal(t *testing.T) {
	s := &hostsServer{
		allowed: []string{"nqn.other"},
		post:    answer(http.StatusBadRequest, `{"detail":"Failed to add host on node n1"}`),
	}
	c := newTestClient(t, s.handler(t))
	_, err := c.EnsureVolumeHost(context.Background(), testHandle, mdsHost)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400 StatusError", err)
	}
}

func TestEnsureVolumeHostRefusesAMalformedHandle(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("the control plane was called") })
	if _, err := c.EnsureVolumeHost(context.Background(), "not-a-handle", mdsHost); err == nil {
		t.Error("a malformed handle was accepted")
	}
	if _, err := c.EnsureVolumeHost(context.Background(), testHandle, ""); err == nil {
		t.Error("an empty host NQN was accepted")
	}
}

func TestRemoveVolumeHostDeletesTheHost(t *testing.T) {
	s := &hostsServer{del: answer(http.StatusNoContent, "")}
	c := newTestClient(t, s.handler(t))
	if err := c.RemoveVolumeHost(context.Background(), testHandle, mdsHost); err != nil {
		t.Fatal(err)
	}
	if len(s.deletes) != 1 || s.deletes[0] != mdsHost {
		t.Errorf("deletes = %v", s.deletes)
	}
}

func TestRemoveVolumeHostCountsAnAbsentHostOrVolumeAsDone(t *testing.T) {
	for name, del := range map[string]func(http.ResponseWriter){
		"host not listed": answer(http.StatusBadRequest, `{"detail":"Host `+mdsHost+` is not in the allowed list"}`),
		"volume gone":     answer(http.StatusNotFound, `{"detail":"Volume not found"}`),
	} {
		s := &hostsServer{del: del}
		c := newTestClient(t, s.handler(t))
		if err := c.RemoveVolumeHost(context.Background(), testHandle, mdsHost); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRemoveVolumeHostReportsAFailure(t *testing.T) {
	s := &hostsServer{del: answer(http.StatusInternalServerError, `{"detail":"boom"}`)}
	c := newTestClient(t, s.handler(t))
	err := c.RemoveVolumeHost(context.Background(), testHandle, mdsHost)
	if err == nil || errors.Is(err, errs.ErrNotFound) {
		t.Errorf("err = %v, want a server error", err)
	}
}
