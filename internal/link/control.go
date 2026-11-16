package link

// ctlRelink asks the agent to dial a replacement link now. The agent acks, dials, moves its
// traffic and retires the session the ask came on. The ack is the delivery receipt that a text
// frame on a lossy channel never had.
var ctlRelink = NewOp[struct{}, struct{}](ctlCat, "link.relink", ServedByAgent)
