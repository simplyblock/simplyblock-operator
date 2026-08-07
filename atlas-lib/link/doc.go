// Package link carries gRPC between the operator and the CSI driver over
// connections the CSI driver opens.
//
// # Why the connection runs backwards
//
// The operator is the one that needs to ask questions — what does this node's
// fabric look like, are this volume's paths up — and the answers live on the
// nodes. The obvious arrangement, an agent listening on every node for the
// operator to dial, needs a port open on each host, a way to discover its
// address, and a NetworkPolicy that permits the operator to reach it. Clusters
// disagree about all three.
//
// So the direction is inverted: the CSI node and controller pods dial the
// operator and hold the connection open, and the operator issues its RPCs back
// down that connection. Nothing listens on a node, nothing has to be
// discovered, and the only reachability the deployment needs is the one it
// already has — pods reaching a Service.
//
// # How a backwards connection still speaks ordinary gRPC
//
// gRPC assumes the side that dialed is the client. Here it is the server, so
// something has to separate the two roles from the direction the TCP connection
// was opened in. That something is a stream multiplexer: each link runs yamux
// over one connection, and yamux sessions are symmetric — either end can open a
// stream, and each end's Accept sees the streams the other opened.
//
// A yamux session is a net.Listener, so both ends run an ordinary grpc.Server
// on it and an ordinary grpc.ClientConn over a dialer that opens streams. No
// tunnelling protocol, no request/response envelopes, no correlation ids:
// deadlines, cancellation, streaming and status codes all work because they are
// the real thing. Services are registered on either side, in either direction.
//
// # Identity
//
// A peer authenticates itself in [linkv1.LinkService.Hello], the one call that
// runs peer-to-hub, because it is the only point at which the hub sees the
// peer's credentials — a hub that merely issued calls would never receive any.
// Until Hello succeeds the session is anonymous, unregistered, and closed when
// the handshake timeout expires.
//
// What a peer claims about itself in Hello is never taken as identity. Every
// node plugin pod shares one ServiceAccount, so a token proves membership in
// the DaemonSet, not which node sent it; a peer trusted to name itself could
// name another node and answer for its volumes. [KubeAuthenticator] therefore
// derives the identity from the token's bound pod claims and rejects a Hello
// that disagrees with them.
//
// # Using it
//
//	hub, _ := link.NewHub(link.HubConfig{Listener: lis, Auth: auth})
//	go hub.Serve(ctx)
//	conn, err := hub.Registry().Conn(link.NodePeer("worker-3"))  // ErrNoSession if down
//
//	agent, _ := link.NewAgent(link.AgentConfig{
//	    Dial:  link.TLSDialer("simplyblock-operator-link:9443", tlsCfg),
//	    ID:    link.NodePeer(os.Getenv("NODE_NAME")),
//	    Token: link.TokenFile("/var/run/secrets/atlas/link/token"),
//	    Register: nodeServer.Register,   // the services this peer answers
//	})
//	go agent.Run(ctx)  // dials, says Hello, serves, reconnects, until ctx ends
//
// A peer that is not currently linked is normal, not exceptional: during a
// rollout every node is briefly gone, and the leader-elected operator replica
// changes underneath them. Lookups for an absent peer fail with [ErrNoSession],
// which classifies as retryable (codes.Unavailable) so a reconciler requeues
// instead of treating it as a fault.
package link