import { describe, expect, it } from 'vitest'
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
})
