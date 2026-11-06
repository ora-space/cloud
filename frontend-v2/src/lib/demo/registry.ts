/**
 * The register of demo mocks for the 11-06 recording.
 *
 * Every mock lives in this directory and is on only when `demoMockEnabled()` (see `flags.ts`) says
 * so; pages never import mock data directly, they ask a module here. Each mock module is listed
 * below with the backend that replaces it. The change that lands that backend deletes the module
 * and its entry together, so this list is always the set of screens the recording still fakes.
 * `registry.test.ts` fails when a module is added without an entry or an entry outlives its module.
 */
export interface DemoMock {
  /** The module in this directory, e.g. `dashboard.ts`. */
  module: string
  /** The recording scene whose screens show it. */
  scene: 'newcomer' | 'lead' | 'ops'
  /** What the mock stands in for. */
  covers: string
  /** The backend change that replaces it and deletes the module. */
  replacedBy: string
}

export const demoMocks: readonly DemoMock[] = [
  {
    module: 'space-usage.ts',
    scene: 'lead',
    covers: 'Workspace token usage, model requests and tool categories for the Dashboard video',
    replacedBy: 'An authorized workspace usage aggregation endpoint with explicit time boundaries',
  },
  { module: 'cdase.ts', scene: 'newcomer', covers: 'SFMEA review workflow and report recording fixture', replacedBy: 'Installed cdase workflow with real Agent execution and persisted review artifacts' },
 {module:'dts.ts',scene:'ops',covers:'DTS simulated receipts after real Cloud task creation',replacedBy:'An authenticated DTS connector with durable external delivery receipts'},
  {
    module: 'task-workflows.ts',
    scene: 'lead',
    covers: 'Handling a task with a workflow: the entry, the requirement, and the run belonging to the task',
    replacedBy: 'Task-owned workflow runs: start one on a task with a requirement, list them, and move the task when they end',
  },
]
