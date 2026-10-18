# Users, Tenants and Members

English | [中文](tenancy.md)

The tenant is the isolation boundary every Cloud resource hangs from. Each tenant has exactly one
user-visible **collaboration space** (Space, table `collab_workspaces`); that space is what users see
and switch between in the interface. Tenant membership (`tenant_memberships`) is the only source of
roles and authorization: which Projects a person sees and which administrative actions they may take
are decided by their current membership state in that tenant.

> "Workspace" means two things in Cloud: the collaboration space (`collab_workspaces`, the Space in
> this document) and the runtime Workspace (`workspaces`, an independent runtime environment under a
> Project, see [Workspace lifecycle](../runtime/workspaces.en.md)). They are different aggregates; the
> documentation calls them Space and Workspace respectively.

## Model

```text
users ──< user_identities          one user may have several external identity mappings
  │
  └──< tenant_memberships >── tenants ── collab_workspaces (exactly one)
         role: admin | member              │
         status: active | disabled         └──< projects ──< workspaces
```

- **Users** come from [identity resolution](authentication.en.md#from-credential-to-user). A user can
  belong to several tenants and switch between them.
- **Tenants and Spaces correspond one to one.** A Space has a globally unique `slug` that can never be
  changed or reused; renaming the Space renames the tenant as well.
- **There are only two roles, `admin` and `member`**, and administrators are peers. A membership is
  `active` or `disabled`; there is no API that physically deletes a member, and disabling keeps every
  historical reference.
- **A current member** means the membership row is `active`, the user is `active` and not deleted,
  and the tenant is `active` and not deleted — all three at once. Every request inside a tenant checks
  this first and fails with `403 membership_required`; operations that need an administrator then
  check the role and fail with `403 admin_required`.

## Creating a tenant

A tenant is always created in one transaction with its first administrator and its sole Space, so a
tenant without an administrator or a Space never exists.

| Path | Entry point | Notes |
|---|---|---|
| Self-serve | `POST /api/v1/tenants` with `name` and `slug` | Any signed-in user may create one and becomes its administrator. This is the only write without tenant scope: the verified identity is the whole authorization. Idempotency records are matched per user (see [idempotency records](../core/persistence.en.md#consistency-tools)) |
| Deployment bootstrap | `cloudctl -command bootstrap` | A deployment operation that creates a tenant and an administrator for a given identity; the Space gets a generated globally unique slug, and running it again creates another tenant |

A `slug` may contain only lowercase letters, digits and hyphens, must start with a letter or digit,
and is at most 64 characters; a clash with an existing Space returns `409 space_slug_conflict`.

A user who signs in for the first time and belongs to no tenant is guided by the frontend to create
their first space. `GET /api/v1/me/spaces` returns all of the user's active spaces for switching.

## Joining a tenant

There are two kinds of deployment with different ways to join, decided by whether a corporate
employee directory is configured (`directory.endpoint`):

**Public deployments (no directory): private links.** Administrators create two kinds of link. The
browser generates the token from 32 random bytes and sends it to Cloud only in the request body; the
database stores only its SHA-256 digest, and the creator assembles the share URL locally:

| Link | Lifetime | Effect |
|---|---|---|
| Invitation (`/tenants/:tid/invitations`) | 7 days, **single use** | Redeeming it (`POST /api/v1/join/invitations/redeem`) makes the invitee a member immediately |
| Application link (`/tenants/:tid/join-links`) | 30 days, reusable | A visitor submits an application (`POST /api/v1/join/requests`); an administrator approves it (`.../join-requests/:rid/approve`) to make them a member, or rejects it |

- Links can be revoked (a versioned `DELETE`). Expired, revoked and already-redeemed tokens all return
  `404 join_link_unavailable`.
- An existing member who applies again gets `409 already_member`; a person has at most one pending
  application.
- Redeeming and applying happen before the user is a member, so their idempotency records are kept
  per user (`join_idempotency_records`). Users see their own applications through
  `GET /api/v1/me/join-requests`.
- There is no public space directory; nobody can search or browse other people's spaces.

**Corporate deployments (directory configured): employee directory checks.** All three private-link
endpoint groups above return `404`; joining goes only through the directory:

1. An administrator searches the employee directory with
   `GET /api/v1/tenants/:tid/people?keyword=…`. Cloud calls the directory server-side, so the browser
   never holds the directory's machine credentials. The keyword is 2 to 100 characters, and only
   employed people are returned, at most 20.
2. The administrator adds a person with `POST /api/v1/tenants/:tid/members/huawei`. Cloud **queries the
   directory again** with the same keyword to confirm the chosen person is still employed, then creates
   the membership by `globalUserId`; the role may be `admin` or `member`.
3. When the added person first signs in through IDaaS, they are linked to this pre-created user through
   `globalUserId` (see [identity resolution](authentication.en.md#from-credential-to-user)).

Administrator authority is checked both before the directory call and before the write, because an
external call separates the two. Employee numbers are for display only and play no part in
authorization or identity matching. When the directory is unavailable the answer is
`503 directory_unavailable`.

## Managing members

`PUT /api/v1/tenants/:tid/members/:uid` (administrators only, versioned) can only change the role or
status of an **existing** member:

- It cannot add a member, and it cannot turn a disabled member back to `active`
  (`409 member_rejoin_required`). Rejoining must go through an invitation, an application or a directory
  add again; those paths reuse and reactivate the original membership row and keep historical
  references.
- The last effective administrator cannot be demoted or disabled (`409 last_admin`). A deferred
  database constraint trigger enforces the same rule, so editing the table directly cannot bypass it.
- Disabling takes effect immediately: every later request and execution admission by the disabled
  member is refused, an open SSE connection closes before its next event, and any runtime control the
  member held is withdrawn (see [Runtime control](../runtime/runtime-control.en.md)). Tasks already
  under way finish their own lifecycle, and audit references are kept.

`GET /api/v1/tenants/:tid/members` lists the tenant's members and is readable by any current member.

## What needs an administrator

Ordinary members can read the tenant's shared resources, create Projects and runtimes, and
collaborate on Issues. Operations that need an administrator include:

- Member management: changing members, invitation and application links, approving applications,
  directory search and add;
- Changing the Space's name and description;
- Deleting a Project, force-stopping a runtime, and reading the tenant-wide resource status overview;
- Changing the space's plugin selection (see [Plugin marketplace](../plugins/marketplace.en.md)).

Runtime content (what is inside a particular Workspace) has finer rules: only its creator and current
administrators may use it; see [Runtime control](../runtime/runtime-control.en.md).

## Related decisions

- [Each visible collaboration space corresponds to one tenant, authorized by tenant membership](https://github.com/ora-space/specs/blob/main/decisions/cloud/tenancy/20260923-one-space-per-tenant.md)
- [Signed-in users provision tenants themselves; the tenant becomes the implicit container of the space](https://github.com/ora-space/specs/blob/main/decisions/cloud/tenancy/0-self-serve-tenant-provisioning.md)
- [The collaboration Space is the sharing, visibility and authorization boundary within a tenant](https://github.com/ora-space/specs/blob/main/decisions/cloud/workspace/0-collab-space-boundary.md)
- [The IDaaS UUID stays the login key; globalUserId becomes the directory association key](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/20260923-huawei-global-id-alias.md)
