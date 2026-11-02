import { beforeEach, expect, it } from 'vitest'
import { useDemoPluginSelections } from './demo-state'

beforeEach(() => {
  useDemoPluginSelections.getState().reset()
  localStorage.clear()
})

it('keeps demo installs scoped and persists them in browser storage', () => {
  const { setSelected } = useDemoPluginSelections.getState()

  setSelected('owner:tenant:space-a', 'demo/sample-hooks', true)
  setSelected('owner:tenant:space-a', 'demo/sample-hooks', true)
  setSelected('owner:tenant:space-b', 'demo/sample-mcp', true)

  expect(useDemoPluginSelections.getState().byScope).toEqual({
    'owner:tenant:space-a': ['demo/sample-hooks'],
    'owner:tenant:space-b': ['demo/sample-mcp'],
  })
  expect(localStorage.getItem('ora-v2-plugin-demo:v1')).toContain(
    'tenant:space-a',
  )

  setSelected('owner:tenant:space-a', 'demo/sample-hooks', false)

  expect(
    useDemoPluginSelections.getState().byScope['owner:tenant:space-a'],
  ).toEqual([])
  expect(
    useDemoPluginSelections.getState().byScope['owner:tenant:space-b'],
  ).toEqual(['demo/sample-mcp'])
})
