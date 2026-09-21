import { describe, expect, it } from 'vitest'
import { db } from '@/mocks/data/store'

const BASE = '/mock-api'
const SLUG = db.workspace.slug

describe('mock API handlers', () => {
  it('returns the seeded workspace list', async () => {
    const res = await fetch(`${BASE}/workspaces`)
    expect(res.status).toBe(200)
    const body = await res.json()
    expect(body.length).toBeGreaterThan(0)
    expect(body.some((w: { slug: string }) => w.slug === SLUG)).toBe(true)
  })

  it('404s for an unknown workspace slug', async () => {
    const res = await fetch(`${BASE}/workspaces/does-not-exist`)
    expect(res.status).toBe(404)
  })

  it('creates and then reads back an issue', async () => {
    const createRes = await fetch(`${BASE}/workspaces/${SLUG}/issues`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ title: 'Fix the flaky test', status: 'todo', priority: 'high' }),
    })
    expect(createRes.status).toBe(201)
    const created = await createRes.json()
    expect(created.title).toBe('Fix the flaky test')
    expect(created.identifier).toMatch(/^ORA-\d+$/)

    const getRes = await fetch(`${BASE}/workspaces/${SLUG}/issues/${created.id}`)
    expect(getRes.status).toBe(200)
    const fetched = await getRes.json()
    expect(fetched.id).toBe(created.id)
  })

  it('patches an issue status and reflects it in the list filter', async () => {
    const issue = db.issues[0]
    const patchRes = await fetch(`${BASE}/workspaces/${SLUG}/issues/${issue.id}`, {
      method: 'PATCH',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ status: 'done' }),
    })
    expect(patchRes.status).toBe(200)

    const listRes = await fetch(`${BASE}/workspaces/${SLUG}/issues?status=done`)
    const list = await listRes.json()
    expect(list.some((i: { id: string }) => i.id === issue.id)).toBe(true)
  })
})
