import { describe, expect, it } from 'vitest'
import { readFileSync, readdirSync } from 'node:fs'
import { join, relative, resolve } from 'node:path'
import enMisc from '@/i18n/locales/en/misc'
import zhMisc from '@/i18n/locales/zh/misc'
import enLanding from '@/i18n/locales/en/landing'
import zhLanding from '@/i18n/locales/zh/landing'
import enAdminSettings from '@/i18n/locales/en/admin/settings'
import zhAdminSettings from '@/i18n/locales/zh/admin/settings'
import { DEFAULT_SITE_NAME, DEFAULT_SITE_SUBTITLE } from '@/config/branding'

describe('Wavelet Innovation locale branding', () => {
  it('positions onboarding as a private internal AI platform in English', () => {
    expect(enMisc.onboarding.admin.welcome.title).toBe(`👋 Welcome to ${DEFAULT_SITE_NAME}`)
    expect(enMisc.onboarding.admin.welcome.description).toContain('private internal platform')
    expect(enMisc.onboarding.admin.welcome.description).not.toContain('Sub2API')
    expect(enMisc.onboarding.admin.welcome.description).not.toContain('VIP')
    expect(enMisc.onboarding.user.welcome.title).toBe(`👋 Welcome to ${DEFAULT_SITE_NAME}`)
  })

  it('positions onboarding as a private internal AI platform in Chinese', () => {
    expect(zhMisc.onboarding.admin.welcome.title).toBe(`👋 欢迎使用 ${DEFAULT_SITE_NAME}`)
    expect(zhMisc.onboarding.admin.welcome.description).toContain('内部 AI 平台')
    expect(zhMisc.onboarding.admin.welcome.description).not.toContain('Sub2API')
    expect(zhMisc.onboarding.admin.welcome.description).not.toContain('VIP')
    expect(zhMisc.onboarding.user.welcome.title).toBe(`👋 欢迎使用 ${DEFAULT_SITE_NAME}`)
  })

  it('uses branded setup and settings defaults in both locales', () => {
    expect(enLanding.setup.title).toBe(`${DEFAULT_SITE_NAME} Setup`)
    expect(enLanding.setup.description).toContain('internal AI platform')
    expect(zhLanding.setup.title).toBe(`${DEFAULT_SITE_NAME} 安装向导`)
    expect(zhLanding.setup.description).toContain('内部 AI 平台')
    expect(enAdminSettings.settings.site.siteNamePlaceholder).toBe(DEFAULT_SITE_NAME)
    expect(enAdminSettings.settings.site.siteSubtitlePlaceholder).toBe(DEFAULT_SITE_SUBTITLE)
    expect(zhAdminSettings.settings.site.siteNamePlaceholder).toBe(DEFAULT_SITE_NAME)
  })

  // 上游每次发版都会新增大量文案，逐条断言跟不上。这里做整目录扫描：
  // 任何 locale 文件里出现展示用的 "Sub2API" 都会失败，把漏改挡在合并阶段。
  // SUB2API_API_KEY 是环境变量标识符，必须放行。
  it('leaves no unbranded product name anywhere in the locale bundles', () => {
    const localesRoot = resolve(process.cwd(), 'src/i18n/locales')

    const localeFiles: string[] = []
    const walk = (dir: string) => {
      for (const entry of readdirSync(dir, { withFileTypes: true })) {
        const full = join(dir, entry.name)
        if (entry.isDirectory()) walk(full)
        else if (entry.name.endsWith('.ts')) localeFiles.push(full)
      }
    }
    walk(localesRoot)
    expect(localeFiles.length).toBeGreaterThan(0)

    const offenders = localeFiles.filter((file) =>
      /Sub2API(?!_API_KEY)/.test(readFileSync(file, 'utf8'))
    )

    expect(offenders.map((file) => relative(localesRoot, file))).toEqual([])
  })

  it('keeps the SUB2API_API_KEY environment variable name untouched', () => {
    const dashboards = ['src/i18n/locales/en/dashboard.ts', 'src/i18n/locales/zh/dashboard.ts'].map(
      (path) => readFileSync(resolve(process.cwd(), path), 'utf8')
    )

    for (const source of dashboards) {
      expect(source).toContain('SUB2API_API_KEY')
      expect(source).toContain('Wavelet Innovation Grok')
    }
  })
})
