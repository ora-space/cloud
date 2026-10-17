# Multiplayer runtime control

[中文](runtime-control.md)

## Authority and scope

A collaboration space is the visible tenant; a Workspace is a project's independent runtime clone and persistent volume. Durable owner, verified creator, initiating actor and current page session are distinct. Active members read shared projects and safe overviews. Only the creator or a current tenant administrator reads runtime content. Unproven historical creators remain unknown and administrator-only. Phase one has no member grant table or live co-editing.

PostgreSQL alone owns business decisions. Transactions contain database work only. Controller dispatch occurs after durable plans; its cloud mode has no business SQLite. Node SQLite is a protected local recovery journal.

## Qualification, lifecycle and force stop

Cloud allocates a page session, reserving acquiring before a durable Node acknowledgement enables held. A stopped runtime without a sandbox can become held directly. Qualification lasts 60 seconds and renews every 20 seconds against PostgreSQL time. Permission, page binding and activity exclusion are independent; ordinary administrator actions cannot steal occupancy.

Expiry, release, disconnect and membership loss close new admission and drain responsibility. Unknown outcomes reconcile by stable identity. Input closure plus settlement of all tickets, clones and ordinary operations precedes idle. Replaying an old idempotent success cannot revive qualification. User control epoch, Controller lease epoch, runtime generation, Node/Host incarnation and execution ID remain distinct. Acceptance and first filesystem mutation are fenced; one conflicting activity per runtime also applies within a page. Started work may settle within its original scope.

Normal lifecycle requests require version and sessionId. Restart is one durable stop-then-start operation and reuses existing data without cloning. Administrator project deletion checks every target atomically.

The separate force-stop endpoint requires reason, target version, explicit impact acknowledgement and an idempotency key. It can coexist with inflight ordinary work, retaining original records and actors. Confirmation requires actual termination and durable late-ensure fencing. Until then restart, handoff and data deletion remain closed. Fresh effect permits last at most ten seconds and cannot outlive the Controller lease; Node binding deadlines are also lease bounded. Historical outcomes remain queryable after new dispatch loses eligibility.

## Deliberately unavailable capabilities

Only administrators change fixed space plugin selections. Existing targets retain pending intent and recovery scans revisit idle conditions under the same maintenance exclusion. Stopped runtimes do not autostart. Production has no real plugin executor, so it reports executor_capability_unavailable and never claims success; simulation is explicitly wired only in development fixtures.

Credential references retain attribution, scope, capabilities, version and availability without secrets. Departure atomically freezes personal/unknown references; team references survive old owner departure; rejoining does not unfreeze them. Original owner FKs and historical references remain. A scoped provider and verified atomic team rebinding are absent. Repository credentials gate no Node work: clone, Agent sessions and delivery talk only to the Git Server, and Nodes never reach the external repository. Cloud file/terminal/Agent product execution remains closed.

## Deployment and evidence

Append-only migrations 0018–0023 preserve history. Historical creator backfill requires unique matching creation evidence; empty new control tables do not prove old runtime idleness. Cloud owns proto, desktop pins an exact commit and generates clients. New unscoped clone admission and production transitional internal JSON management paths are closed; historical reconciliation remains. Controller only dials out; minicloud and old listeners stay retired.

Cloud management RPC requires TLS 1.3 client authentication and the configured Controller identity URI. Deployment files use CLOUD_CONTROL_CERTIFICATE_FILE, CLOUD_CONTROL_PRIVATE_KEY_FILE, CLOUD_CONTROL_CLIENT_CA_FILE and CLOUD_CONTROL_CONTROLLER_IDENTITY. Management credentials never enter workload directories.

Cloud tests prove PostgreSQL/HTTP/gRPC decisions, not physical termination. Desktop tests prove durable local execution fences and real TLS; cluster acceptance proves Docker, Git, process identity and volume boundaries. Complete fault recovery, credential provisioning and upgrade/rollback combinations need separate evidence. The six ADRs remain approved while those obligations are incomplete.
