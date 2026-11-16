// Package ports holds the interfaces the core uses. lifecycle drives tasks through four of them:
// Store (state), Boxes (a task's box and the agent terminal in it), Agent (one agent CLI) and
// Secrets (credential placeholders). Enrollments stores machine pairings. Each names only domain
// types and the plain values here.
package ports
