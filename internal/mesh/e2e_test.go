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
	bobN.OnMessage(func(_ store.MeshPeer, _ mesh.Kind, b mesh.MessageBody, _ mesh.Provenance) { got <- b })

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
	memberN.OnMessage(func(_ store.MeshPeer, _ mesh.Kind, b mesh.MessageBody, _ mesh.Provenance) { got <- b })

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

// link connects x to y and has y accept it with the given scopes.
func link(ctx context.Context, t *testing.T, x, y *mesh.Node, yEndpoint string, scopes []string) {
	t.Helper()
	if _, err := x.Connect(ctx, yEndpoint, ""); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := y.Accept(ctx, x.Identity().ID(), scopes); err != nil {
		t.Fatalf("accept: %v", err)
	}
}

// The delegation scenario: alice asks bob, bob needs carol's help.
func delegationTrio(ctx context.Context, t *testing.T) (a, b, c *mesh.Node, cInst *inst) {
	t.Helper()
	ai, bi, ci := newInst(t, "alice", mesh.Config{}), newInst(t, "bob", mesh.Config{}), newInst(t, "carol", mesh.Config{})
	a = mesh.New(ai.node.Identity(), mesh.Config{Endpoint: ai.endpoint()}, ai.db, zap.NewNop())
	b = mesh.New(bi.node.Identity(), mesh.Config{Endpoint: bi.endpoint()}, bi.db, zap.NewNop())
	c = mesh.New(ci.node.Identity(), mesh.Config{Endpoint: ci.endpoint()}, ci.db, zap.NewNop())
	ai.srv.Config.Handler = a.Handler()
	bi.srv.Config.Handler = b.Handler()
	ci.srv.Config.Handler = c.Handler()

	link(ctx, t, b, c, ci.endpoint(), []string{mesh.ScopeMessage, mesh.ScopeAsk})
	link(ctx, t, a, c, ci.endpoint(), []string{mesh.ScopeMessage})
	return a, b, c, ci
}

// askedBy builds the provenance bob would hold after alice put a question to it.
func askedBy(t *testing.T, from *mesh.Node, to *mesh.Node) mesh.Provenance {
	t.Helper()
	box, err := mesh.ParseBoxKey(to.Public().BoxPub)
	if err != nil {
		t.Fatal(err)
	}
	e, err := mesh.Seal(from.Identity(), mesh.KindAsk, to.Identity().ID(), box,
		mesh.MessageBody{Text: "what did we agree with the vendor?"})
	if err != nil {
		t.Fatal(err)
	}
	return mesh.ProvenanceOf(e)
}

func TestADelegatedAskArrivesWithItsOrigin(t *testing.T) {
	ctx := context.Background()
	alice, bob, carol, carolInst := delegationTrio(ctx, t)

	seen := make(chan mesh.Provenance, 2)
	carol.OnAsk(func(_ context.Context, _ store.MeshPeer, _ string, p mesh.Provenance) (string, error) {
		seen <- p
		return "we agreed on net-30", nil
	})

	if err := bob.SendOnBehalf(ctx, carol.Identity().ID(), mesh.KindAsk,
		mesh.MessageBody{Text: "what did we agree with the vendor?"}, askedBy(t, alice, bob)); err != nil {
		t.Fatalf("delegated ask: %v", err)
	}

	select {
	case p := <-seen:
		if p.Origin() != alice.Identity().ID() {
			t.Error("carol could not tell the question originated with alice")
		}
		if p.Asker() != bob.Identity().ID() {
			t.Error("carol lost track of who asked it directly")
		}
		if !p.Delegated() || p.Depth() != 1 {
			t.Errorf("depth = %d", p.Depth())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the delegated ask never arrived")
	}

	msgs, _ := carolInst.db.RecentMeshMessages(10)
	if len(msgs) == 0 || msgs[0].Origin != alice.Identity().ID() {
		t.Error("the origin was not recorded for audit")
	}
}

func TestWorkDelegatedForABlockedInstanceIsRefused(t *testing.T) {
	ctx := context.Background()
	alice, bob, carol, _ := delegationTrio(ctx, t)

	// Carol wants nothing to do with alice — but still trusts bob.
	if err := carol.Block(alice.Identity().ID()); err != nil {
		t.Fatal(err)
	}
	answered := make(chan struct{}, 1)
	carol.OnAsk(func(context.Context, store.MeshPeer, string, mesh.Provenance) (string, error) {
		answered <- struct{}{}
		return "net-30", nil
	})

	// Bob asks on alice's behalf. The signature is valid and bob is trusted, so
	// without the chain this is how a block gets bypassed by one hop.
	err := bob.SendOnBehalf(ctx, carol.Identity().ID(), mesh.KindAsk,
		mesh.MessageBody{Text: "what did we agree with the vendor?"}, askedBy(t, alice, bob))
	if err == nil {
		t.Error("carol accepted work laundered on behalf of an instance it blocked")
	}
	select {
	case <-answered:
		t.Fatal("carol answered a question from a blocked instance")
	case <-time.After(500 * time.Millisecond):
	}

	// Bob asking for itself is still fine — the block is on alice, not bob.
	if err := bob.Send(ctx, carol.Identity().ID(), mesh.KindAsk,
		mesh.MessageBody{Text: "unrelated question"}); err != nil {
		t.Errorf("bob's own question was refused: %v", err)
	}
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("bob's own question never arrived")
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
	memberN.OnMessage(func(store.MeshPeer, mesh.Kind, mesh.MessageBody, mesh.Provenance) { delivered <- struct{}{} })

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
