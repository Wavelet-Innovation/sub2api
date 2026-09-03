export const DEFAULT_SITE_NAME = 'Wavelet Innovation API Platform'
export const DEFAULT_SITE_SUBTITLE = 'Internal access to approved AI models and services'
export const DEFAULT_DOCUMENT_TITLE = DEFAULT_SITE_NAME
export const ORGANIZATION_GITHUB_URL = 'https://github.com/Wavelet-Innovation'

export function resolveSiteName(siteName?: string | null): string {
  return typeof siteName === 'string' && siteName.trim()
    ? siteName.trim()
    : DEFAULT_SITE_NAME
}
