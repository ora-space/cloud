import { describe, expect, it } from 'vitest'
import { emptyDraft, nextTimes, summary, targetFromDraft, validateDraft } from './schedule-model'

describe('schedule rules', () => {
  it('uses task timezone, skips an elapsed time, and rolls the year over', () => {
    const rule = emptyDraft().rule
    expect(nextTimes(rule, new Date('2026-12-31T01:00:00Z')).map(d => d.toISOString())).toEqual(['2027-01-01T01:00:00.000Z', '2027-01-02T01:00:00.000Z', '2027-01-03T01:00:00.000Z'])
  })
  it('skips weekends and supports several selected weekdays', () => {
    const rule = { ...emptyDraft().rule, kind: 'weekdays' as const }
    expect(nextTimes(rule, new Date('2026-10-30T02:00:00Z'))[0]?.toISOString()).toBe('2026-11-02T01:00:00.000Z')
    expect(nextTimes({ ...rule, kind: 'weekly', days: [1, 5] }, new Date('2026-10-30T02:00:00Z')).map(d => d.toISOString())).toEqual(['2026-11-02T01:00:00.000Z', '2026-11-06T01:00:00.000Z', '2026-11-09T01:00:00.000Z'])
  })
  it('skips a nonexistent DST wall time instead of displaying a shifted hour', () => {
    const rule = { ...emptyDraft().rule, time: '02:30', timezone: 'America/New_York' }
    expect(nextTimes(rule, new Date('2026-03-08T00:00:00Z'))[0]?.toISOString()).toBe('2026-03-09T06:30:00.000Z')
  })
  it('anchors hourly recurrence to creation, not each render', () => {
    const rule = { ...emptyDraft().rule, kind: 'hours' as const, hours: 6 }
    expect(nextTimes(rule, new Date('2026-10-29T08:30:00Z'), new Date('2026-10-29T01:00:00Z')).map(d => d.toISOString())).toEqual(['2026-10-29T13:00:00.000Z', '2026-10-29T19:00:00.000Z', '2026-10-30T01:00:00.000Z'])
  })
})
describe('execution target boundaries',()=>{
 it('builds conversation and workflow requests and rejects task references without interpreting their parameters',()=>{
  const draft={...emptyDraft(),name:'简报',prompt:'汇总变化'}
  expect(validateDraft(draft)).toEqual({})
  expect(targetFromDraft(draft)).toEqual({kind:'chat',prompt:'汇总变化',agent:'',model:''})
  expect(validateDraft({...draft,kind:'task'})).toHaveProperty('target')
  expect(()=>targetFromDraft({...draft,kind:'task'})).toThrow('引用目标只读')
 })
 it('freezes the published snapshot of the chosen workflow, in a required project',()=>{
  const data={projects:[{id:'p',name:'Demo',description:''}],workflows:[{id:'wf',name:'周报流程',version:'v3',snapshotId:'snap-3'}]}
  const draft={...emptyDraft(),name:'周报',kind:'workflow' as const,workflowId:'wf',snapshotId:'snap-1',input:'整理'}
  expect(validateDraft(draft,data)).toEqual({projectId:'工作流要在项目中运行，请选择项目'})
  expect(validateDraft({...draft,projectId:'p',workflowId:'gone'},data)).toEqual({workflowId:'该工作流已不可用或没有发布版本，请重新选择'})
  expect(validateDraft({...draft,projectId:'p'},data)).toEqual({})
  // The version a save freezes is the one published now, not the one frozen before.
  expect(targetFromDraft({...draft,projectId:'p'},data)).toEqual({kind:'workflow',workflowId:'wf',snapshotId:'snap-3',input:'整理'})
  expect(summary({...draft,projectId:'p'},data)).toContain('运行工作流「周报流程」（v3）')
 })
})
