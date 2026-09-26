# Frontend

Ora Cloud's web frontend: React 19, TypeScript, Vite, Tailwind CSS 4, TanStack Query. The root
`AGENTS.md` applies to this directory as well; this file adds the frontend's engineering rules.
`frontend/README.md` documents the toolchain, commands and the contract-generation invariants.
Configuration in `package.json`, `tsconfig.*.json`, `.oxlintrc.json`, `.prettierrc.json`,
`vite.config.ts`, `knip.json` and `.jscpd.json` is authoritative; `npm run check` is the gate.

The rules below are deliberately independent of the current directory layout. The layout will
change; the rules apply to whatever layout exists, and the gate scripts under `scripts/` discover
modules from the tree rather than from a hard-coded list.

## Cohesion and coupling

1. **One reason to change per module.** A module (a directory that directly contains hand-written
   source) owns one concern: a domain, a screen, a boundary adapter, a set of presentational
   primitives, or test scaffolding. If a module's README needs "and" to describe what it does,
   split it.
2. **Dependencies point one way.** Every module's README names what it depends on and what may
   depend on it. Lower layers (framework-free utilities, boundary adapters, generated clients)
   never import higher layers (components, screens, composition root). `import/no-cycle` fails the
   build on any cycle; do not work around it with lazy imports or barrel tricks.
3. **Cross-module imports go through the `@/` alias and a module's public surface.** Relative
   imports (`./x`) stay inside a module; `../` is banned by lint. Import only what a module
   exports on purpose; do not reach into another module's internals because the file happens to
   be reachable.
4. **Boundaries are adapters, not scattered calls.** HTTP, storage, timers, `window`, environment
   and third-party SDKs are wrapped once in a module that owns that boundary. Components and hooks
   call the wrapper, never `axios`, `fetch`, `localStorage` or `Date.now()` directly. This is what
   makes tests hermetic (swap the adapter) and keeps policy (auth headers, retries, base URL) in
   one place.
5. **Server state lives in TanStack Query; UI state lives in React.** Do not copy server responses
   into `useState`, context or a store. Do not fetch inside presentational components. Derived
   data is computed, not stored.
6. **Explicit inputs, explicit outputs.** Props, hook parameters and function arguments are typed
   and named for what they mean, not how they are used. Prefer a small typed object over positional
   parameters past three. Prefer two functions or a typed union over a boolean switch parameter.
   `any`, non-null assertions and unchecked index access are lint/type errors, not style choices.
7. **Split before you generalize.** When a file starts serving two callers differently, split it
   into two cohesive units instead of adding options. Extract a helper only when it names an
   invariant, centralizes error-prone policy, or has genuine reuse; a helper used once that hides
   what the caller does is worse than inline code.

## Size and complexity limits (enforced by lint)

| Metric | Limit | Rule |
| --- | --- | --- |
| Lines per file (excluding blanks and comments) | 800 | `max-lines` |
| Lines per function or component | 100 | `max-lines-per-function` |
| Cyclomatic complexity per function | 15 | `complexity` |
| Block nesting depth | 4 | `max-depth` |
| Parameters per function | 4 | `max-params` |
| JSX nesting depth | 8 | `react/jsx-max-depth` |
| Nested ternaries | 0 | `no-nested-ternary` |

These are ceilings, not targets: a component at 100 lines or a file at 800 is already large for
this codebase, and the split should usually happen well before the lint fires. Prefer a small
typed object over positional parameters past three even though four is the hard limit. Hitting a
limit is a design signal, not an invitation to disable the rule. Split the file, extract
a hook, lift a sub-component, or replace branching with a lookup table. Suppressions
(`// oxlint-disable-next-line`) require a comment stating why the invariant does not apply and are
reviewed as design decisions.

## Code smells that fail the gate

- **Duplication**: `jscpd` fails on any clone of 8+ lines / 60+ tokens across hand-written source.
  Extract the shared piece into the module that owns the concept.
- **Dead code**: `knip` fails on unused files, exports, types and dependencies. Delete rather than
  keep "for later"; git remembers.
- **Cycles**: `import/no-cycle`.
- **Leaky effects**: `react/exhaustive-deps` is an error; a missing dependency is a bug, not a
  warning. Effects that synchronize with external systems belong in a boundary hook, not in
  components.
- **Unstable identities**: components defined inside components, array-index keys, and objects or
  functions recreated per render and passed as props to memoized children.
- **Silent failure**: unhandled promises (`no-floating-promises`), swallowed errors, `console.log`
  left behind. Errors surface through the query/mutation state or a typed error boundary; logging
  is `console.warn`/`console.error` with context, never raw objects containing tokens or personal
  data.
- **Stringly typed state**: status flags as loose strings, magic numbers, boolean prop soup. Use
  union types, named constants and typed variants (`cva`) instead.

## Documentation

Documentation is part of the code change, not a follow-up. The gate verifies presence and shape;
review verifies substance.

1. **Every module has `README.md` (中文) and `README.en.md` (English).** Both files say the same
   thing. Each covers: what the module is responsible for and what it is not; a table of its
   files with one line each; which modules it depends on and which may depend on it; invariants a
   maintainer must not break; anything non-obvious about testing it. Keep it under one screen;
   link to deeper docs instead of inlining them. `scripts/check-modules.mjs` fails the build when
   either file is missing, has no heading, or is too short to be a description.
2. **Every exported symbol has a JSDoc block.** `scripts/check-exports-documented.mjs` fails the
   build when an export has no JSDoc or when the description merely restates the identifier.
   Say what the symbol is for, what it guarantees, and what callers must not assume. For
   components, document the contract of the props that are not obvious from their types. Types
   are already in the signature; do not repeat them in `@param {Type}` tags.
3. **Comments carry information or do not exist.** Write a comment when the code cannot say it:
   why this approach, which invariant a line protects, which upstream quirk is being worked
   around, what will break if the order changes. Never narrate what the code visibly does, never
   leave commented-out code, never add a comment to satisfy a reviewer rather than a reader. A
   comment that could be replaced by a better name should be.
4. **Docs move with the code.** On pull requests, `scripts/check-modules.mjs --base <ref>` fails
   when a module's implementation changed but its two READMEs did not. If the change genuinely
   leaves the documentation accurate (a rename, a formatting change, a bug fix inside a documented
   contract), add a commit trailer `Docs-Unchanged: <reason>`; the reason is printed in CI and
   reviewed. Do not use the trailer to defer documentation.
5. **Package-level docs are `frontend/README.md` / `README.en.md`**: toolchain, commands, contract
   generation, and links to module READMEs. Update them when a command, gate or invariant changes.

## Tests

1. **Every module with implementation files contains tests**, and every behavior change ships
   with a test change in the same module. `scripts/check-modules.mjs` enforces both: structurally
   on every run, and per change on pull requests (waivable with `Tests-Unchanged: <reason>`, same
   rules as the docs trailer).
2. **Coverage thresholds are global and only go up.** `vitest` fails below the thresholds in
   `vite.config.ts` (lines, functions, branches, statements). Raise them when the codebase exceeds
   them; never lower them to merge.
3. **Tests are hermetic and deterministic.** No network, no real backend, no timers left running,
   no test-order dependence. Replace boundaries with the doubles in the test scaffolding module
   (currently `src/test/`); do not mock internal modules. Test behavior through the public
   surface: rendered output, emitted requests, returned values, thrown errors.
4. **Test names state the invariant.** `it('aborts the request when the caller signal aborts')`,
   not `it('works')`. Compare complete values where practical so a failure shows the whole
   difference.
5. **Test files sit next to what they test** (`x.test.ts` beside `x.ts`) and count toward the
   module's test requirement. Shared fixtures and doubles live in the scaffolding module and have
   their own tests.

## Contract and generated code

- `src/api/` is generated from `api/openapi.json` by orval and is never hand-edited; the gate
  regenerates it and fails on drift. Shared runtime code that generated files import lives in
  a hand-written module (currently `src/lib/api-client.ts`), which is where HTTP policy goes.
- When the backend contract changes, run `task frontend:generate` from the repository root and
  commit `api/openapi.json` and `frontend/src/api` together with the Go change.
- Generated code is exempt from lint, format, duplication, documentation and coverage checks. It
  is not exempt from type checking; if a strict compiler flag rejects generated output, fix the
  hand-written mutator or adapter types rather than relaxing the flag.

## Dependencies and the lock file

- `package-lock.json` is regenerated only by the npm version CI uses: `engines.npm` in
  `package.json` names the minimum and `.npmrc` sets `engine-strict`, so an older npm refuses to
  install. If `npm install` fails with `EBADENGINE`, upgrade npm (`npm install -g npm@latest`) or
  Node 24 itself; never lower the range. The lock has broken three times (e27f86d, 7a8e7ec,
  and the 8511e39 removal of zustand) because an older npm silently drops the optional peer
  dependencies of `@napi-rs/wasm-runtime` (`@emnapi/core`, `@emnapi/runtime`) that npm 11.19+
  requires, and CI's `npm ci` then fails with "lock file out of sync".
- After any `package.json` change, run `npm ci` before committing: it is the same command CI
  runs and the only reliable proof that `package.json` and the lock agree. Commit both files
  together, never hand-edit the lock, and keep one registry in it: every entry resolves to the
  official `registry.npmjs.org`. To install through a mirror, set npm's `registry` locally: npm's
  default `replace-registry-host=npmjs` redirects the lock's URLs at install time, and mirror URLs
  must never be written into the lock.

## Security and data

- No secrets in the frontend, ever: no tokens, keys or passwords in source, `.env` files that are
  committed, or build output. Authentication material arrives from the backend at runtime and is
  held only where the boundary adapter needs it.
- Treat URL parameters, `localStorage`, `postMessage` and every API response as untrusted input;
  validate at the boundary and pass typed values inward.
- Never render untrusted HTML. `dangerouslySetInnerHTML` requires a documented sanitizer and a
  review note explaining why plain rendering is impossible.
- Logs and error reports exclude tokens and personal data.

## Change workflow

1. Read this file, the module READMEs you will touch, and the existing tests before designing a
   change. Decide which module owns the new behavior; if none does, create one with its READMEs
   and tests in the same change.
2. Implement the smallest coherent change. Keep code, JSDoc, module READMEs and tests together;
   do not split "code now, docs later" across pull requests.
3. Run `npm run format`, then `npm run check` (or `task frontend:check` from the repository root,
   which also verifies generated-client drift). Before opening a pull request, run
   `npm run check:modules -- --base origin/main` to see what the CI change check will demand.
4. Never weaken a lint rule, coverage threshold, size limit or gate script to make the build pass.
   Fix the cause, or add the narrowest suppression with a comment stating the invariant that makes
   it safe.
5. Review `git diff` for leftover `console.log`, TODOs without an owner, commented-out code, and
   README tables that no longer match the files in the directory.
