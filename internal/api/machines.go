package api

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"time"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/auth"
	"github.com/IniZio/outpost/internal/domain"
)

// Pairing timing: how long a code lives and how often an agent should ask.
const (
	pairTTL      = 10 * time.Minute
	pairInterval = 2 * time.Second
)

func (s *Server) machineRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/machines", s.watch(s.machines))
	m.HandleFunc("DELETE /api/machines/{id}", s.act(s.deleteMachine))
	m.HandleFunc("POST /api/machines/pair", s.open(s.pair))
	m.HandleFunc("POST /api/machines/pair/collect", s.open(s.collect))
	m.HandleFunc("POST /api/machines/confirm", s.act(s.confirm))
}

var roleNames = map[domain.MachineRole]string{domain.RoleSandbox: "sandbox", domain.RoleTool: "tool"}

func machineView(m domain.Machine, online bool) client.Machine {
	return client.Machine{
		ID: string(m.ID), Name: m.Name, Owner: string(m.Owner), Role: roleNames[m.Role],
		Slots: m.Slots, Exec: m.Exec, Online: online,
	}
}

func (s *Server) machines(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	all, err := s.cfg.Machines.Machines(r.Context())
	if err != nil {
		return fmt.Errorf("api: machines: %w", err)
	}
	out := []client.Machine{}
	for _, m := range all {
		if m.Machine.Owner == p.Login {
			out = append(out, machineView(m.Machine, m.Online))
		}
	}
	reply(w, http.StatusOK, out)
	return nil
}

func (s *Server) deleteMachine(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	id := domain.MachineID(r.PathValue("id"))
	all, err := s.cfg.Machines.Machines(r.Context())
	if err != nil {
		return fmt.Errorf("api: machines: %w", err)
	}
	for _, m := range all {
		if m.Machine.ID != id || m.Machine.Owner != p.Login {
			continue
		}
		if _, err := s.cfg.Machines.DeleteMachine(r.Context(), id); err != nil {
			return fmt.Errorf("api: delete machine: %w", err)
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	return errNotFound
}

// codeAlphabet has no vowels and no letters that look like digits, so a code is easy to read aloud and spells no word.
const codeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

func userCode() (domain.UserCode, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("api: random: %w", err)
	}
	c := make([]byte, 0, 9)
	for i, x := range b {
		if i == 4 {
			c = append(c, '-')
		}
		c = append(c, codeAlphabet[int(x)%len(codeAlphabet)])
	}
	return domain.UserCode(c), nil
}

func (s *Server) pair(w http.ResponseWriter, r *http.Request) error {
	var in client.PairRequest
	if err := decode(w, r, &in); err != nil {
		return err
	}
	req := domain.PairRequest{Name: in.Name, Slots: in.Slots, Exec: in.Exec}
	if copy(req.Key[:], in.Key) != len(req.Key) || len(in.Key) != len(req.Key) {
		return fmt.Errorf("%w: the key is a 32-byte ed25519 public key", domain.ErrBadPairing)
	}
	for role, name := range roleNames {
		if name == in.Role {
			req.Role = role
		}
	}
	var device [32]byte
	_, _ = rand.Read(device[:])
	code, err := userCode()
	if err != nil {
		return err
	}
	now := s.cfg.Clock.Now()
	e, err := req.Begin(device, code, now, pairTTL)
	if err != nil {
		return err //nolint:wrapcheck // the domain's refusal, worded for people
	}
	if err := s.cfg.Pairing.Begin(r.Context(), e); err != nil {
		return fmt.Errorf("api: begin pairing: %w", err)
	}
	reply(w, http.StatusCreated, client.PairTicket{
		UserCode: string(code), DeviceCode: device[:], IntervalSec: int(pairInterval / time.Second), Expires: e.Expires,
	})
	return nil
}

func (s *Server) collect(w http.ResponseWriter, r *http.Request) error {
	var in client.Collect
	if err := decode(w, r, &in); err != nil {
		return err
	}
	var device [32]byte
	if copy(device[:], in.DeviceCode) != len(device) || len(in.DeviceCode) != len(device) {
		return domain.ErrUnknownPairing
	}
	e, ok, err := s.cfg.Pairing.Collect(r.Context(), device, s.cfg.Clock.Now())
	if err != nil {
		return fmt.Errorf("api: collect: %w", err)
	}
	if !ok {
		reply(w, http.StatusAccepted, client.Collected{State: "pending"})
		return nil
	}
	reply(w, http.StatusOK, client.Collected{State: "enrolled", MachineID: string(e.Machine)})
	return nil
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var in client.Confirm
	if err := decode(w, r, &in); err != nil {
		return err
	}
	e, err := s.cfg.Pairing.Confirm(r.Context(), domain.UserCode(in.UserCode), p.Login, s.cfg.Clock.Now())
	if err != nil {
		return fmt.Errorf("api: confirm: %w", err)
	}
	m := e.Row()
	m.ID = e.Machine
	reply(w, http.StatusOK, machineView(m, false))
	return nil
}
