package githubissues

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

// TestToken is the token of the services built for tests, exported so tests
// outside the package can look for it.
const TestToken = fakeToken

// NewSnapshotTransportForTest returns the SnapshotTransport of a Service whose
// token is TestToken and accepted, with https://github.com/{repo} registered,
// and whose requests go to server whatever host they name. It lets tests
// outside the package see exactly what a caller of SnapshotTransport sees.
func NewSnapshotTransportForTest(t *testing.T, server, repo string) http.RoundTripper {
	t.Helper()
	target, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	svc := newTestService(t, "", "2027-03-01")
	svc.repoHTTPClient = noRedirectClient(&http.Client{Transport: toServer{target: target}, Timeout: 5 * time.Second})
	store := newFakeRepoStore()
	store.register(testUUID(1), "https://github.com/"+repo)
	svc.store = store
	svc.state.markAccepted("octocat", testNow)
	return svc.SnapshotTransport()
}
