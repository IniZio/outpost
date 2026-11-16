// Package ops is the catalogue of typed operations that cross the machine link: their names, who
// serves them, the capability they need, and the request and reply types. It holds no behaviour.
// internal/machine serves the ServedByAgent ops, the server packages serve the ServedByServer ones
// and call the rest. It is the single list of operations: a new op is one declaration here and one
// handler where it is served, and nothing else to keep in step.
//
// The machine ops and the box ops (box.go) are here. The hc attach, sync, listen and exec
// operations are declared in this package when their handlers are built.
package ops

import (
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
)

var cat = link.NewCatalogue()

// Catalogue is what both ends build their link.Registry from.
func Catalogue() *link.Catalogue { return cat }

type (
	// PingReq carries a token the agent echoes, so a caller knows the answer is to its own call.
	PingReq struct {
		Token string `json:"token"`
	}
	// PingResp echoes the token.
	PingResp struct {
		Token string `json:"token"`
	}

	// StatusResp is what the agent says about itself. Role and Exec are what the agent was paired
	// as, and the server compares them with its own row.
	StatusResp struct {
		Build string             `json:"build"`
		Role  domain.MachineRole `json:"role"`
		Exec  bool               `json:"exec"`
	}

	// ReportLine is one line of the machine report stream: the agent writes one JSON object per
	// line for as long as the stream is open.
	ReportLine struct {
		MemFree  uint64  `json:"mem_free"`
		DiskFree uint64  `json:"disk_free"`
		Pressure float64 `json:"pressure"`
	}
)

// Ops the server opens on a machine.
var (
	// Ping asks the agent to echo a token. It proves the agent serves ops, not only that it is linked.
	Ping = link.NewOp[PingReq, PingResp](cat, "machine.ping", link.ServedByAgent)
	// Status asks the agent what it is. It is read-only.
	Status = link.NewOp[struct{}, StatusResp](cat, "machine.status", link.ServedByAgent)
)

// Ops an agent opens on the server.
var (
	// Report is held open by the agent. The server reads ReportLine values from it. It is
	// resumable: the agent reopens it on every new link.
	Report = link.NewResumableOp[struct{}, struct{}](cat, "machine.report", link.ServedByServer)
)
