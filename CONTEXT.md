# Vyshka: process and roadmap glossary

Wire-level terms (action, event, snapshot, context, namespace, identity, session, manifest)
are defined normatively in `spec/protocol.md` and are not repeated here. This file holds
only the words the roadmap and the working process lean on.

## Roadmap states

- **Shipped**: merged on `main`, graded by a conformance suite or the browser test, and
  where a game is involved, demonstrated against a live DayZ server.
- **Committed**: the protocol specifies it, a milestone's definition of done requires it, or
  an issue is open for it. It will be built; only the order can move.
- **Proposed**: a candidate under discussion. A proposed item must leave that state at the
  next roadmap review: it becomes committed, parked, or dropped.
- **Parked**: wanted in principle, not scheduled, and carrying a named trigger. When the
  trigger fires the item becomes committed. A parked item with no trigger is a defect in
  the roadmap.
- **Tabled**: wanted in principle, with no trigger. Only a roadmap review can reopen it,
  and nothing else on the roadmap may depend on it. Distinct from parked precisely because
  it has no trigger; distinct from dropped because it is still wanted.
- **Dropped**: decided against and recorded under "Not on the roadmap" with a one-line
  reason, so it is not re-proposed by accident.

## Work units

- **Slice**: one issue, one branch, the spec change first where there is one, the
  conformance suite grading the new behavior, the two-round adversarial review, and a
  changelog entry in the landing commit. The only unit in which committed work lands.
- **Spike**: a measurement of an engine or platform limit, kept under `spikes/` with its
  results, done before anything is designed around the number. A spike produces a fact,
  not a feature.
- **Tripwire**: an observable condition, written next to a deferred decision, whose
  occurrence reopens that decision. The 256 KiB snapshot body cap is the canonical one.

## People

- **Operator**: the person, or small team, who installs one hub and enrolls a handful of
  game servers in it for their own community, with a few admins who do not all hold the
  same trust. The game servers usually live on other boxes than the hub, so the Plugin API
  is reachable over the public internet. The design persona
  through protocol 1.0. A hosting outfit running many servers for other people is a
  different persona and is out of scope until 1.0.
- **Installation**: one hub and its database, with every server enrolled in it. The unit
  the operator owns and the widest scope any token can have today.
