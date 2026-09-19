# Test fixtures

Synthetic `bd export` JSONL, shared by the package tests. Every record is made up.

- `multirepo/`: a clean three-repo graph. `hq` holds the goal bead `hq-g1`, a human gate and
  orchestration records. `api` has a milestone, an epic, closed history, in-progress work and a gate
  by title. `web` has an epic under the same goal, blockers in `api` and `hq`, a gate by metadata and
  a closed epic under a goal that is not loaded (`hq-g2`). Infra records (a convoy, a molecule, an
  ephemeral wisp and a template) must all be dropped.
- `problems/`: what the loader must report without failing: an open blocking cycle (`a-1`, `a-2`), a
  closed one (`a-3`, `a-4`), dangling edges from `a-5`, and `a-5` defined again in repo `b`.
