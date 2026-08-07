import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import {
  DEFAULT_DOCUMENT_TITLE,
  DEFAULT_SITE_NAME,
  DEFAULT_SITE_SUBTITLE,
  ORGANIZATION_GITHUB_URL,
  resolveSiteName,
} from '@/config/branding'

describe('Wavelet Innovation branding defaults', () => {
  it('uses the approved internal platform identity', () => {
    expect(DEFAULT_SITE_NAME).toBe('Wavelet Innovation API Platform')
    expect(DEFAULT_SITE_SUBTITLE).toBe('Internal access to approved AI models and services')
    expect(DEFAULT_DOCUMENT_TITLE).toBe('Wavelet Innovation API Platform')
  })

  it('uses a configured site name when present and the Wavelet default otherwise', () => {
    expect(resolveSiteName('Custom Internal Gateway')).toBe('Custom Internal Gateway')
    expect(resolveSiteName('   ')).toBe(DEFAULT_SITE_NAME)
    expect(resolveSiteName(undefined)).toBe(DEFAULT_SITE_NAME)
  })

  it('uses Wavelet Innovation in static browser and logo metadata', () => {
    const indexHtml = readFileSync(resolve(process.cwd(), 'index.html'), 'utf8')
    const logoSvg = readFileSync(resolve(process.cwd(), 'public/logo.svg'), 'utf8')

    expect(indexHtml).toContain(`<title>${DEFAULT_DOCUMENT_TITLE}</title>`)
    expect(indexHtml).not.toContain('<title>Sub2API')
    expect(logoSvg).toContain(`<title id="title">${DEFAULT_SITE_NAME}</title>`)
  })

  it('points general website GitHub links to the Wavelet Innovation organization', () => {
    expect(ORGANIZATION_GITHUB_URL).toBe('https://github.com/Wavelet-Innovation')

    const linkSurfaces = [
      'src/views/HomeView.vue',
      'src/views/KeyUsageView.vue',
      'src/components/layout/AppHeader.vue',
    ].map((path) => readFileSync(resolve(process.cwd(), path), 'utf8'))

    for (const source of linkSurfaces) {
      expect(source).toContain('ORGANIZATION_GITHUB_URL')
      expect(source).not.toContain('https://github.com/Wei-Shaw/sub2api')
    }
  })
})
