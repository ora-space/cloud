# Ora Cloud Documentation

English | [中文](README.md)

These are Ora Cloud's cross-module architecture, contract and operations documents. Implementation details inside a
package live in that directory's `README.md`; the authoritative source for business decisions is the ADRs in
[specs](https://github.com/ora-space/specs/tree/main/decisions/cloud); endpoint fields are defined by
[`api/openapi.json`](../api/openapi.json).

## Where to start

| I want to… | Read first |
|---|---|
| Understand the whole system | [Architecture overview](architecture.en.md) |
| Run it on my machine | [Local development](development/local-development.en.md) |
| Change the code | [Changing the code](development/changing-the-code.en.md), then the document for the domain you are changing |
| Deploy or operate it | [Deployment and operations](operations/deployment.en.md), [Configuration reference](operations/configuration.en.md) |

## All documents

**Core**

- [Authoritative persistence and the transaction model](core/persistence.en.md): the Store, the global lock, errors, post-commit publishing, business hooks, consistency tools, migrations
- [HTTP API conventions](core/http-api.en.md): the route allowlist, credentials, request decoding, errors, pagination, SSE, the contract generation chain

**Identity and tenancy**

- [Internal credentials and service trust](identity/authentication.en.md): dual JWTs, the Controller's mTLS identity, from credential to user
- [Gateway](identity/gateway.en.md): external login, browser sessions, the proxy, login providers
- [Users, tenants and members](identity/tenancy.en.md): tenants and Spaces, the two ways to join, member management

**Runtime**

- [Projects, Workspaces and lifecycle operations](runtime/workspaces.en.md): aggregates, durable operations and their steps, effects
- [Runtime use permissions, exclusive control and force-stop](runtime/runtime-control.en.md)
- [Controller integration: the internal control contract](runtime/controller-integration.en.md): lease, submission identity, the signal stream, each service

**Repositories**

- [Git Server and Project repositories](repositories/git-server.en.md): repository Bindings and Operations, listeners, authorization, ref rules
- [The external Git line](repositories/external-git.en.md): Repo Executor, Credential Store, sync and explicit publish, the credential lifecycle, authorization and failure convergence

**Collaboration**

- [The Issue board](collaboration/issues.en.md)
- [Issue collaboration: @ targets, interactions and execution intent](collaboration/interactions.en.md)

**Agent runs**

- [Agent IssueRuns: an Agent run from start to finish](agent-runs/issue-runs.en.md)
- [Threads: the conversation of an Agent run](agent-runs/threads.en.md)
- [Bounded startup: waiting limits and settling unknown outcomes](agent-runs/startup-settlement.en.md)
- [Agent Run residual reclamation: Cloud authority and evidence](agent-runs/residual-reclamation.en.md)
- [Revisions: delivery, verified registration and resume](agent-runs/revisions.en.md)
- [Session reads: session lists, archives and model request statistics](agent-runs/session-reads.en.md)

**Models**

- [Personal model connections, run bindings and short-lived grants](models/model-access.en.md)
- [model-gateway: the personal key and model proxy service](models/model-gateway.en.md)

**Plugins and workflows**

- [The plugin marketplace and space plugins](plugins/marketplace.en.md)
- [Workflows](workflows/workflows.en.md)

**Operations**

- [Configuration reference](operations/configuration.en.md)
- [Deployment and operations](operations/deployment.en.md)
- [Shared deployment egress](operations/deployment-egress.en.md)

**Development**

- [Local development](development/local-development.en.md)
- [Testing and gates](development/testing.en.md)
- [Changing the code](development/changing-the-code.en.md)

## Maintenance rules

- `docs/` holds only documents maintained together with the code over time. Plans, progress, implementation logs, research
  notes and acceptance records stay out of the repository and live in the issue tracking the work.
- Every document has a Chinese version (`x.md`) and an English version (`x.en.md`) linked to each other below the title; both
  are updated together in the same change.
- Documents describe facts verifiable against the current code and link the relevant ADRs instead of retelling the decision
  process; decisions that are `proposed`, or `approved` but not yet implemented, are never described as implemented.
