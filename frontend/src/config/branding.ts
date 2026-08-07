export const DEFAULT_SITE_NAME = 'Wavelet Innovation API Platform'
export const DEFAULT_SITE_SUBTITLE = 'Internal access to approved AI models and services'
export const DEFAULT_DOCUMENT_TITLE = DEFAULT_SITE_NAME

export function resolveSiteName(siteName?: string | null): string {
  return typeof siteName === 'string' && siteName.trim()
    ? siteName.trim()
    : DEFAULT_SITE_NAME
}
