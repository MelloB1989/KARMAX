// Package fleet is the agent fleet's session manager: fleetd, the reconciler
// and system of record, and fleetctl, its CLI. See docs/AGENT-FLEET.md.
//
// It is its own module on purpose. It needs nothing from KARMAX and KARMAX
// links nothing from it; the two meet only at the CLI, a webhook route and
// a dashboard's data files.
package fleet
