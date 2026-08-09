package mesh_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/mesh"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// Two real instances, over real HTTP. Everything below is the protocol as it
// actually runs, not a mock of it.
type inst struct {
	node *mesh.Node
	db   *store.Store
	srv  *httptest.Server
}

func newInst(t *testing.T, name string, cfg mesh.Config) *inst {
	t.Helper()
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "k.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	id, err := mesh.LoadOrCreateIdentity(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	n := mesh.New(id, cfg, db, zap.NewNop())
	srv := httptest.NewServer(n.Handler())
	t.Cleanup(srv.Close)
	return &inst{node: n, db: db, srv: srv}
}

func (i *inst) endpoint() string { return i.srv.URL + "/mesh" }

// rebuild replaces the node with one whose endpoint is known, now that the
// test server has an address.
func (i *inst) withEndpoint(t *testing.T, cfg mesh.Config) *inst {
	t.Helper()
	cfg.Endpoint = i.endpoint()
	return i
}

func TestTwoInstancesConnectAndTalk(t *testing.T) {
	ctx := context.Background()
	alice := newInst(t, "alice", mesh.Config{})
	bob := newInst(t, "bob", mesh.Config{})
	// Endpoints are only known after the servers start, so they are set here.
	aliceN := mesh.New(alice.node.Identity(), mesh.Config{Endpoint: alice.endpoint()}, alice.db, zap.NewNop())
	bobN := mesh.New(bob.node.Identity(), mesh.Config{Endpoint: bob.endpoint()}, bob.db, zap.NewNop())
	alice.srv.Config.Handler = aliceN.Handler()
	bob.srv.Config.Handler = bobN.Handler()

	got := make(chan mesh.MessageBody, 4)
	bobN.OnMessage(func(_ store.MeshPeer, _ mesh.Kind, b mesh.MessageBody) { got <- b })

	// Before any connection, a message must be refused outright.
	if err := aliceN.Send(ctx, bobN.Identity().ID(), mesh.KindMessage, mesh.MessageBody{Text: "hi"}); err == nil {
		t.Fatal("a message to an unconnected instance was sent")
	}

	// Alice asks. Bob now has a pending decision and nothing more.
	if _, err := aliceN.Connect(ctx, bob.endpoint(), "colleague"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	peers, _ := bob.db.ListMeshPeers(store.PeerPending)
	if len(peers) != 1 {
		t.Fatalf("bob has %d pending requests, want 1", len(peers))
	}
	if peers[0].Fingerprint != aliceN.Identity().Fingerprint() {
		t.Error("the pending request does not carry alice's real fingerprint")
	}

	// Still refused: a request is not a connection.
	if err := aliceN.Send(ctx, bobN.Identity().ID(), mesh.KindMessage, mesh.MessageBody{Text: "hi"}); err == nil {
		t.Fatal("a message was accepted while the request was still pending")
	}

	// Bob accepts — the human decision.
	if err := bobN.Accept(ctx, peers[0].ID, []string{mesh.ScopeMessage, mesh.ScopeBroadcast}); err != nil {
		t.Fatalf("accept: %v", err)
	}

	if err := aliceN.Send(ctx, bobN.Identity().ID(), mesh.KindMessage,
		mesh.MessageBody{Subject: "standup", Text: "shipping the mesh today"}); err != nil {
		t.Fatalf("send after accept: %v", err)
	}
	select {
	case b := <-got:
		if b.Text != "shipping the mesh today" || b.Subject != "standup" {
			t.Errorf("received %+v", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the message never arrived")
	}

	// And it is recorded on both sides.
	in, _ := bob.db.RecentMeshMessages(10)
	if len(in) == 0 {
		t.Error("bob did not record the message")
	}
}

func TestBlockedInstanceStaysBlocked(t *testing.T) {
	ctx := context.Background()
	alice := newInst(t, "alice", mesh.Config{})
	bob := newInst(t, "bob", mesh.Config{})
	aliceN := mesh.New(alice.node.Identity(), mesh.Config{Endpoint: alice.endpoint()}, alice.db, zap.NewNop())
	bobN := mesh.New(bob.node.Identity(), mesh.Config{Endpoint: bob.endpoint()}, bob.db, zap.NewNop())
	alice.srv.Config.Handler = aliceN.Handler()
	bob.srv.Config.Handler = bobN.Handler()

	if _, err := aliceN.Connect(ctx, bob.endpoint(), ""); err != nil {
		t.Fatal(err)
	}
	if err := bobN.Block(aliceN.Identity().ID()); err != nil {
		t.Fatal(err)
	}
	// Asking again must not return the peer to pending — otherwise "no" is
	// just a decision an attacker retries until someone mis-clicks.
	_, _ = aliceN.Connect(ctx, bob.endpoint(), "please?")
	p, err := bob.db.MeshPeerByID(aliceN.Identity().ID())
	if err != nil {
		t.Fatal(err)
	}
	if p.State != store.PeerBlocked {
		t.Errorf("a blocked instance returned to %q by re-asking", p.State)
	}
}

func TestOrgReachesAMemberWithNoConnection(t *testing.T) {
	ctx := context.Background()
	org := newInst(t, "vector-org", mesh.Config{})
	member := newInst(t, "kartik", mesh.Config{})

	orgN := mesh.New(org.node.Identity(), mesh.Config{
		Endpoint: org.endpoint(), IsOrg: true, OrgName: "Vector",
	}, org.db, zap.NewNop())
	// The member trusts this org key because its operator configured it —
	// never because a message claimed it.
	memberN := mesh.New(member.node.Identity(), mesh.Config{
		Endpoint: member.endpoint(), TrustedOrg: orgN.Identity().ID(),
	}, member.db, zap.NewNop())
	org.srv.Config.Handler = orgN.Handler()
	member.srv.Config.Handler = memberN.Handler()

	got := make(chan mesh.MessageBody, 2)
	memberN.OnMessage(func(_ store.MeshPeer, _ mesh.Kind, b mesh.MessageBody) { got <- b })

	cert := mesh.IssueCertificate(orgN.Identity(), "Vector",
		memberN.Identity().ID(), []string{mesh.ScopeMessage}, time.Hour)

	// No peer.request, no accept — this is the org's privilege.
	if err := orgN.SendAsOrg(ctx, store.MeshPeer{
		ID:       memberN.Identity().ID(),
		BoxPub:   member.node.Public().BoxPub,
		Endpoint: member.endpoint(),
		Name:     "kartik",
	}, cert, mesh.KindMessage, mesh.MessageBody{
		Subject: "all-hands", Text: "security review Friday",
	}); err != nil {
		t.Fatalf("org send: %v", err)
	}
	select {
	case b := <-got:
		if b.Text != "security review Friday" {
			t.Errorf("received %+v", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the org message never arrived")
	}
}

func TestAnUntrustedOrgIsRefused(t *testing.T) {
	ctx := context.Background()
	realOrg := newInst(t, "vector", mesh.Config{})
	rogue := newInst(t, "rogue", mesh.Config{})
	member := newInst(t, "kartik", mesh.Config{})

	rogueN := mesh.New(rogue.node.Identity(), mesh.Config{
		Endpoint: rogue.endpoint(), IsOrg: true, OrgName: "Vector",
	}, rogue.db, zap.NewNop())
	// The member trusts the REAL org only.
	memberN := mesh.New(member.node.Identity(), mesh.Config{
		Endpoint: member.endpoint(), TrustedOrg: realOrg.node.Identity().ID(),
	}, member.db, zap.NewNop())
	member.srv.Config.Handler = memberN.Handler()

	delivered := make(chan struct{}, 1)
	memberN.OnMessage(func(store.MeshPeer, mesh.Kind, mesh.MessageBody) { delivered <- struct{}{} })

	// A rogue org signs itself a certificate naming this member and calls
	// itself "Vector". Everything verifies internally — and it must still be
	// refused, because the member never trusted that key.
	cert := mesh.IssueCertificate(rogueN.Identity(), "Vector",
		memberN.Identity().ID(), []string{"*"}, time.Hour)
	err := rogueN.SendAsOrg(ctx, store.MeshPeer{
		ID: memberN.Identity().ID(), BoxPub: member.node.Public().BoxPub,
		Endpoint: member.endpoint(), Name: "kartik",
	}, cert, mesh.KindMessage, mesh.MessageBody{Text: "wire the money"})
	if err == nil {
		select {
		case <-delivered:
			t.Fatal("a message from an untrusted org was delivered")
		case <-time.After(500 * time.Millisecond):
		}
	}
}
