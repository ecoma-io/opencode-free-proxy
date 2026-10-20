# docs

Guides and investigation records. Public artifacts — English, dated, with
source citations.

## Guides

| Doc                                                                      | What it covers                                                                                                                                                     |
| ------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| [configuration.md](configuration.md)                                     | The full `OCFP_CONFIG` document: schema tables, defaults, `${VAR}` interpolation, hot-reload semantics, validation errors                                          |
| [recovery-semantics.md](recovery-semantics.md)                           | Who may recover from what across Injector → OFP → RPGW: the request-state model, the proof boundary, safe egress failover, health, and the cross-service contracts |
| [deployment.md](deployment.md)                                           | Docker / Compose, config mounts, bootstrap env, healthz, graceful shutdown, production considerations                                                              |
| [architecture.md](architecture.md)                                       | Request pipeline, immutable runtime generations, process-wide state lifecycles, package layout                                                                     |
| [adversarial-verification-matrix.md](adversarial-verification-matrix.md) | Executable proof matrix for the recovery contract, its guard symbols, and gate output                                                                              |

## Investigation records

Recon notes that would otherwise get lost between sessions.

| File                                                       | What it covers                                                                                                                                                                                                                                             |
| ---------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [recon-opencode-ua.md](recon-opencode-ua.md)               | How the official opencode CLI builds its compound User-Agent, the live binary capture, the per-segment version chains, and the sync design they feed                                                                                                       |
| [recon-session-continuity.md](recon-session-continuity.md) | Live upstream check: continuation requests with different session / project ids — error behavior, server-side memory, validation, and prompt-cache keying                                                                                                  |
| [recon-ip-switch.md](recon-ip-switch.md)                   | Live upstream check: one conversation continued across a mid-stream egress IP change with fully rotated identity — blocking behavior, continuity, prompt-cache keying                                                                                      |
| [recon-free-tier-gate.md](recon-free-tier-gate.md)         | Live upstream check: what the free-tier gate actually validates — the session-id counter limit, the moved `1.18` version floor, and which forged signals stopped mattering                                                                                 |
| [recon-abuse-ip-flag.md](recon-abuse-ip-flag.md)           | Live upstream check (2026-10-20): the new "detected abusive traffic from this IP" 403 — a deterministic per-IP flag on the egress pool, not a fingerprint change; probe results per upstream IP and the v2.1.1 source diff that found no new client signal |
