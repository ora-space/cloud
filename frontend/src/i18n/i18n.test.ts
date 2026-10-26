import { afterEach, describe, expect, it } from 'vitest'
import { activeLocale, setLocale } from '@/i18n/i18n-instance'
import { isLocale, rememberLocale, storedLocale } from '@/i18n/locale'
import { composeTranslationResources, type TranslationBundle } from '@/i18n/resource-bundle'
import { translationResources } from '@/i18n/resources'

/** A well-formed bundle with one plain key. */
function bundle(key: string, chinese: string, english: string): TranslationBundle {
  return { 'zh-CN': { [key]: chinese }, 'en-US': { [key]: english } }
}

describe('composeTranslationResources', () => {
  it('merges features into one map per locale', () => {
    const resources = composeTranslationResources({
      first: bundle('a.one', '一', 'One'),
      second: bundle('b.two', '二', 'Two'),
    })
    expect(resources['zh-CN']).toEqual({ 'a.one': '一', 'b.two': '二' })
    expect(resources['en-US']).toEqual({ 'a.one': 'One', 'b.two': 'Two' })
  })

  it('names the locale and keys a bundle is missing', () => {
    const lopsided: TranslationBundle = {
      'zh-CN': { 'a.one': '一', 'a.two': '二' },
      'en-US': { 'a.one': 'One' },
    }
    expect(() => composeTranslationResources({ feature: lopsided })).toThrow(
      /Translation keys differ in feature: en-US missing \[a\.two\]/,
    )
  })

  it('rejects one key claimed by two features', () => {
    expect(() =>
      composeTranslationResources({
        first: bundle('shared.key', '一', 'One'),
        second: bundle('shared.key', '二', 'Two'),
      }),
    ).toThrow(/Duplicate translation key shared\.key: first and second/)
  })

  it('requires every plural form the locale uses', () => {
    // English has `one` and `other`; Chinese has only `other`.
    const missingEnglishPlural: TranslationBundle = {
      'zh-CN': { 'a.count_other': '{{count}} 个' },
      'en-US': { 'a.count_other': '{{count}} items' },
    }
    expect(() => composeTranslationResources({ feature: missingEnglishPlural })).toThrow(
      /Missing en-US plural form: a\.count_one/,
    )
  })

  it('accepts a bundle that declares every required plural form', () => {
    const resources = composeTranslationResources({
      feature: {
        'zh-CN': { 'a.count_other': '{{count}} 个' },
        'en-US': { 'a.count_one': '{{count}} item', 'a.count_other': '{{count}} items' },
      },
    })
    // The composed map's key type is the declared union, which for this
    // single-feature case is just the plural pair, so read it as a plain map.
    const english: Record<string, string> = resources['en-US']
    expect(english['a.count_one']).toBe('{{count}} item')
  })
})

describe('the shipped resources', () => {
  it('cover both locales with identical logical keys', () => {
    const chinese = Object.keys(translationResources['zh-CN']).toSorted()
    const english = Object.keys(translationResources['en-US']).toSorted()
    expect(english).toEqual(chinese)
    expect(chinese.length).toBeGreaterThan(0)
  })

  it('give every node kind a palette label and description', () => {
    // Widened to a plain map: the key is built from a variable, so the
    // composed map's literal-key type cannot index it.
    const chinese: Record<string, string> = translationResources['zh-CN']
    for (const kind of [
      'start',
      'agent',
      'condition',
      'aggregator',
      'tool',
      'junction',
      'human',
      'loop',
      'loopExit',
      'iteration',
      'subflow',
      'output',
    ]) {
      expect(chinese[`workflows.node.${kind}.label`]).toBeTruthy()
      expect(chinese[`workflows.node.${kind}.description`]).toBeTruthy()
    }
  })
})

describe('locale storage', () => {
  afterEach(() => {
    window.localStorage.clear()
  })

  it('narrows only the supported locales', () => {
    expect(isLocale('zh-CN')).toBe(true)
    expect(isLocale('en-US')).toBe(true)
    expect(isLocale('fr-FR')).toBe(false)
    expect(isLocale(null)).toBe(false)
  })

  it('falls back to zh-CN for an unset or unknown stored value', () => {
    expect(storedLocale()).toBe('zh-CN')
    window.localStorage.setItem('ora.locale', 'fr-FR')
    expect(storedLocale()).toBe('zh-CN')
  })

  it('round-trips a remembered locale', () => {
    rememberLocale('en-US')
    expect(storedLocale()).toBe('en-US')
  })
})

describe('the i18n instance', () => {
  afterEach(() => {
    setLocale('zh-CN')
  })

  it('starts in the default locale and switches on request', async () => {
    expect(activeLocale()).toBe('zh-CN')
    setLocale('en-US')
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(activeLocale()).toBe('en-US')
    expect(document.documentElement.lang).toBe('en-US')
  })
})
