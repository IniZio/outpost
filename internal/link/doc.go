// Package link is the transport between the Outpost server and the outpost-agent on each attached
// machine. It owns one thing: a typed, authenticated, restartable request and byte-stream channel.
// It does not know what any operation does. The operations are values declared in internal/ops
// (names, request and reply types, who serves them, which capability they need). The agent serves
// them from internal/machine, and the server calls them through its own ports.
//
// # Shape
//
//	agent --WebSocket (wss, no auth header)--> ingress --> server
//	    text frames : hello, challenge, proof, welcome   (handshake.go; never used after welcome)
//	    binary      : one yamux session                   (session.go)
//
// A yamux stream is one op. Frame 1, written by the opener, is the request. Frame 2, written by
// the side that serves the op, is the reply or an error. A stream op then carries raw bytes.
//
// # Rules the code keeps
//
//   - Either side opens streams and both sides serve from a Registry. There is no request id: a
//     stream is a request.
//   - Operations are Go values, so a call site cannot name an op, send the wrong request type or
//     read the wrong reply type. The catalogue is the only list of operations.
//   - The kind of an op is its drain policy. A finite op (Op, StreamOp) is waited for during a
//     drain up to the budget, cut after it, and never replayed. A resumable op (ResumableOp) is cut
//     at once and reopened on the next link by Hold, so its handler must adopt state it already
//     holds.
//   - Control traffic (relink) is an ordinary built-in op in the same dispatcher, so it is
//     acknowledged and ordered with data. After the WebSocket is wrapped for yamux no text frame
//     can be sent.
//   - One keepalive: yamux's. WebSocket pings are off.
//   - A machine's link generation lives in the store (Presence). Going offline applies only when
//     the closing link's generation is still the stored one, so a late close of an old link cannot
//     mark a newer one offline, even across two server processes during a deploy.
//   - The package never reads the wall clock itself. Time comes through domain.Clock and sleeping
//     through Sleeper.
package link
